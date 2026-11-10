package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodestat"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// statLine writes one collector-shaped JSONL line for a node. The fixture is
// deliberately built from nodestat's own metric constants so a rename in the
// contract cannot leave the test passing against a stale name.
// statClient is an in-memory runtime.Client for the node-stats path: NodeList
// (with a failure mode), Info and SetNodeLabel are the only methods it needs.
type statClient struct {
	runtime.Client
	nodes   []runtime.Node
	nodeErr error

	// mu guards the call logs: the re-assert loop appends from its own
	// goroutine while the test reads, which the race detector rightly
	// flags. A test that reads these without the accessors below is a bug.
	mu         sync.Mutex
	labelCalls []string
	restarts   []string
}

// labelSnapshot returns a copy of the recorded label writes.
func (c *statClient) labelSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.labelCalls...)
}

// restartSnapshot returns a copy of the recorded service restarts.
func (c *statClient) restartSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.restarts...)
}

// clearLabels resets the log between phases of a test.
func (c *statClient) clearLabels() {
	c.mu.Lock()
	c.labelCalls = nil
	c.mu.Unlock()
}

func (c *statClient) NodeList(context.Context) ([]runtime.Node, error) {
	return c.nodes, c.nodeErr
}

func (c *statClient) Info(context.Context) (runtime.Info, error) {
	return runtime.Info{Name: "self"}, nil
}

func (c *statClient) SetNodeLabel(_ context.Context, id, key, value string) error {
	c.mu.Lock()
	c.labelCalls = append(c.labelCalls, id+":"+key+":"+value)
	c.mu.Unlock()
	return nil
}

func (c *statClient) ServiceRestart(_ context.Context, id string) error {
	c.mu.Lock()
	c.restarts = append(c.restarts, id)
	c.mu.Unlock()
	return nil
}

func statLine(host, role string, ts int64, load float64) string {
	return `{"resourceMetrics":[{"resource":{"attributes":[{"key":"host.name","value":{"stringValue":"` + host + `"}},{"key":"node.role","value":{"stringValue":"` + role + `"}}]},` +
		`"scopeMetrics":[{"metrics":[` +
		`{"name":"` + nodestat.MetricLoad1 + `","gauge":{"dataPoints":[{"asDouble":` + strconv.FormatFloat(load, 'f', -1, 64) + `,"timeUnixNano":"` + strconv.FormatInt(ts, 10) + `000000000","attributes":[{"key":"host.name","value":{"stringValue":"` + host + `"}}]}]}},` +
		`{"name":"` + nodestat.MetricMemTotalMB + `","gauge":{"dataPoints":[{"asDouble":1839,"timeUnixNano":"` + strconv.FormatInt(ts, 10) + `000000000","attributes":[{"key":"host.name","value":{"stringValue":"` + host + `"}}]}]}},` +
		`{"name":"` + nodestat.MetricMemAvailMB + `","gauge":{"dataPoints":[{"asDouble":120,"timeUnixNano":"` + strconv.FormatInt(ts, 10) + `000000000","attributes":[{"key":"host.name","value":{"stringValue":"` + host + `"}}]}]}},` +
		`{"name":"` + nodestat.MetricCores + `","gauge":{"dataPoints":[{"asDouble":1,"timeUnixNano":"` + strconv.FormatInt(ts, 10) + `000000000","attributes":[{"key":"host.name","value":{"stringValue":"` + host + `"}}]}]}}` +
		`]}]}]}`
}

func TestSyncLeaderLabel_GainSetsClearsAndSkipsNoops(t *testing.T) {
	// The fake's Info().Name is "self", which is the name the daemon trusts
	// first; the node list uses the same name, as a real swarm does.
	host := "self"

	dc := &statClient{nodes: []runtime.Node{
		{ID: "self", Hostname: host, Role: "manager", IsLeader: true},
		{ID: "other", Hostname: "peer-1", Role: "manager"},
	}}

	if !syncLeaderLabel(context.Background(), dc, true, zerolog.Nop()) {
		t.Fatal("gaining leadership reported no change; the collector would not be re-placed")
	}
	if calls := dc.labelSnapshot(); len(calls) != 1 || calls[0] != "self:"+runtime.LeaderNodeLabel+":true" {
		t.Fatalf("label calls = %v, want one true on self", calls)
	}

	// Already labelled: a restart must not rewrite the node spec.
	dc.nodes[0].Labels = map[string]string{runtime.LeaderNodeLabel: "true"}
	dc.clearLabels()
	if syncLeaderLabel(context.Background(), dc, true, zerolog.Nop()) {
		t.Fatal("a second gain reported a change; the collector would restart on every daemon restart")
	}
	if calls := dc.labelSnapshot(); len(calls) != 0 {
		t.Fatalf("label calls = %v, want none when already set", calls)
	}

	// Leadership lost: cleared (an empty value removes the key in the adapter).
	if !syncLeaderLabel(context.Background(), dc, false, zerolog.Nop()) {
		t.Fatal("losing leadership reported no change")
	}
	if calls := dc.labelSnapshot(); len(calls) != 1 || calls[0] != "self:"+runtime.LeaderNodeLabel+":" {
		t.Fatalf("label calls = %v, want one clear on self", calls)
	}

	// Never touches another node while that node carries no label: the label names
	// the leader, not every manager. (A STALE label on another node is cleared —
	// see the next test — because two matches make the collector's constraint
	// ambiguous.)
	for _, c := range dc.labelSnapshot() {
		if c[:4] != "self" {
			t.Fatalf("label call %q targeted a node other than self", c)
		}
	}
}

// TestSyncLeaderLabel_StartupOnLeaderReasserts: a daemon that STARTS while it
// already holds leadership must still assert the label. WatchSwarmLeadership
// emits its initial state, so the first control-loop pass runs the same gain path;
// this drives that call directly — the label is absent (a failed write before the
// restart, a hand-removed label) and must be written.
func TestSyncLeaderLabel_StartupOnLeaderReasserts(t *testing.T) {
	dc := &statClient{nodes: []runtime.Node{
		{ID: "self", Hostname: "self", Role: "manager", IsLeader: true},
	}}
	if !syncLeaderLabel(context.Background(), dc, true, zerolog.Nop()) {
		t.Fatal("a leader starting without the label reported no change; the collector would be unschedulable")
	}
	if calls := dc.labelSnapshot(); len(calls) != 1 || calls[0] != "self:"+runtime.LeaderNodeLabel+":true" {
		t.Fatalf("label calls = %v, want the label stamped on self", calls)
	}
}

// TestSyncLeaderLabel_ClearsStaleLabelOnAnotherNode: the clear on losing
// leadership is best-effort, so a former leader can keep the label. When the new
// leader asserts its own, it must also clear the stale copy — otherwise the
// collector's constraint matches two nodes and the scheduler may pick the one whose
// disk the leader's daemon never reads.
func TestSyncLeaderLabel_ClearsStaleLabelOnAnotherNode(t *testing.T) {
	dc := &statClient{nodes: []runtime.Node{
		{ID: "self", Hostname: "self", Role: "manager", IsLeader: true,
			Labels: map[string]string{runtime.LeaderNodeLabel: "true"}},
		{ID: "old", Hostname: "nxt-sw-4-m", Role: "manager",
			Labels: map[string]string{runtime.LeaderNodeLabel: "true"}},
	}}
	if !syncLeaderLabel(context.Background(), dc, true, zerolog.Nop()) {
		t.Fatal("a stale label on another node was not cleared")
	}
	if calls := dc.labelSnapshot(); len(calls) != 1 || calls[0] != "old:"+runtime.LeaderNodeLabel+":" {
		t.Fatalf("label calls = %v, want exactly the stale label cleared on old", calls)
	}

	// A follower must not police other nodes: the leader owns that decision. Its
	// own (absent) label is written as a clear; the leader's label is left alone.
	dc2 := &statClient{nodes: []runtime.Node{
		{ID: "self", Hostname: "self", Role: "manager"},
		{ID: "leader", Hostname: "nxt-sw-5-m", Role: "manager", IsLeader: true,
			Labels: map[string]string{runtime.LeaderNodeLabel: "true"}},
	}}
	syncLeaderLabel(context.Background(), dc2, false, zerolog.Nop())
	for _, c := range dc2.labelSnapshot() {
		if c != "self:"+runtime.LeaderNodeLabel+":" {
			t.Fatalf("a follower wrote %q — only the leader manages other nodes' labels", c)
		}
	}
}

// TestKeepLeaderLabel_ReassertsWhileLeader: one write per election is not enough
// once the label is a hard constraint — a write that failed during the election
// leaves the collector with no matching node. The loop retries until leadership
// ends and then stops.
func TestKeepLeaderLabel_ReassertsWhileLeader(t *testing.T) {
	prev := leaderLabelReassertInterval
	leaderLabelReassertInterval = 5 * time.Millisecond
	defer func() { leaderLabelReassertInterval = prev }()

	dc := &statClient{nodes: []runtime.Node{
		{ID: "self", Hostname: "self", Role: "manager", IsLeader: true},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { keepLeaderLabel(ctx, dc, zerolog.Nop()); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for len(dc.labelSnapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if len(dc.labelSnapshot()) == 0 {
		cancel()
		t.Fatal("the re-assert loop never wrote the label")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the re-assert loop did not stop when leadership ended")
	}
}

func TestSyncLeaderLabel_NilClientIsSafe(t *testing.T) {
	if syncLeaderLabel(context.Background(), nil, true, zerolog.Nop()) {
		t.Fatal("a nil client cannot have changed anything")
	}
}

// TestReplaceNodeStatCollector_ForcesOneRestart: a placement PREFERENCE is only
// read when a task is created, so the leadership label moving must be followed by
// exactly one force-update of the collector service.
func TestReplaceNodeStatCollector_ForcesOneRestart(t *testing.T) {
	dc := &statClient{nodes: []runtime.Node{{ID: "self", Hostname: "self", Role: "manager"}}}
	dc.nodes[0].Labels = map[string]string{}
	replaceNodeStatCollector(context.Background(), dc, zerolog.Nop())
	if restarts := dc.restartSnapshot(); len(restarts) != 1 || restarts[0] != "observability_otel-collector" {
		t.Fatalf("restarts = %v, want exactly the collector service", restarts)
	}
	// Nil client is a no-op, never a panic: standalone mode has no collector.
	replaceNodeStatCollector(context.Background(), nil, zerolog.Nop())
}

// TestTailNodeStatsOnce_UpsertsAndPrunes drives the leader's whole read path
// against a real store and a real file.
func TestTailNodeStatsOnce_UpsertsAndPrunes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nodestat.jsonl")
	body := statLine("n1", "manager", 1000, 9.96) + "\n" + statLine("n2", "worker", 1000, 0.4) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	dc := &statClient{nodes: []runtime.Node{
		{ID: "1", Hostname: "n1", Role: "manager"},
		{ID: "2", Hostname: "n2", Role: "worker"},
	}}
	r := nodestat.NewReader(path)
	res := tailNodeStatsOnce(ctx, r, st, dc, zerolog.Nop())
	if len(res.Stats) != 2 {
		t.Fatalf("read stats = %d, want 2", len(res.Stats))
	}

	rows, err := st.ListNodeStats(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Hostname != "n1" || rows[0].Load1 != 9.96 || rows[0].MemTotalMB != 1839 || rows[0].Cores != 1 {
		t.Fatalf("n1 row = %+v", rows[0])
	}

	// An unchanged file writes nothing new and keeps exactly two rows.
	res = tailNodeStatsOnce(ctx, r, st, dc, zerolog.Nop())
	if len(res.Stats) != 0 {
		t.Fatalf("second tick parsed %d stats, want 0", len(res.Stats))
	}
	rows, _ = st.ListNodeStats(ctx)
	if len(rows) != 2 {
		t.Fatalf("rows after idle tick = %d, want 2", len(rows))
	}

	// n2 leaves the swarm: its row is pruned; n1's stays.
	dc.nodes = dc.nodes[:1]
	tailNodeStatsOnce(ctx, r, st, dc, zerolog.Nop())
	rows, _ = st.ListNodeStats(ctx)
	if len(rows) != 1 || rows[0].Hostname != "n1" {
		t.Fatalf("rows after prune = %+v, want only n1", rows)
	}
}

// TestTailNodeStatsOnce_NodeListFailureKeepsRows is the guard against a
// transient manager outage wiping the panel.
func TestTailNodeStatsOnce_NodeListFailureKeepsRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nodestat.jsonl")
	if err := os.WriteFile(path, []byte(statLine("n1", "manager", 1, 1)+"\n"), 0o600); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	dc := &statClient{nodes: []runtime.Node{{ID: "1", Hostname: "n1"}}}
	r := nodestat.NewReader(path)
	tailNodeStatsOnce(ctx, r, st, dc, zerolog.Nop())

	dc.nodeErr = context.DeadlineExceeded
	tailNodeStatsOnce(ctx, r, st, dc, zerolog.Nop())
	rows, _ := st.ListNodeStats(ctx)
	if len(rows) != 1 {
		t.Fatalf("rows = %d after a failed node list, want the row kept", len(rows))
	}
}

func TestNodeStatFilePath_EnvOverride(t *testing.T) {
	t.Setenv(nodeStatPathEnv, "/tmp/custom-nodestat.jsonl")
	if got := nodeStatFilePath(); got != "/tmp/custom-nodestat.jsonl" {
		t.Fatalf("path = %q", got)
	}
	t.Setenv(nodeStatPathEnv, "")
	if got := nodeStatFilePath(); got != nodeStatDefaultPath {
		t.Fatalf("path = %q, want the /var/stack default", got)
	}
}

// writeProcTree lays out the minimum /proc a sampler needs. The cli test lives
// outside the nodestat package, so it cannot borrow that package's fixture
// helper; the bytes are the same shape a Linux node produces.
func writeProcTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"proc/loadavg": "1.25 1.10 0.90 2/200 4242\n",
		"proc/meminfo": "MemTotal:        1883124 kB\nMemAvailable:     204800 kB\n",
		"proc/cpuinfo": "processor	: 0\nmodel name	: Xeon\n\nprocessor	: 1\nmodel name	: Xeon\n",
		// comm holds a space on purpose: the sampler's field counting must
		// survive it on the leader's own daemon too.
		"proc/4242/stat":   "4242 (pmcluster serve) S 1 4242 4242 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 1 0 100 0 0\n",
		"proc/4242/status": "Name:	pmcluster\nVmRSS:	   932104 kB\n",
	}
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	return root
}

// TestNodeStatSamplerExportsOnLeader: a daemon that IS the leader still emits
// its own sample. The leader is the node running the collector, and the
// collector's node-stats pipeline ends in a `file` exporter — there is no loop
// to avoid, so silence is pure loss: the panel rendered three rows for four
// nodes. This drives one tick with a client that reports this node as Leader
// and asserts the exported gauge reaches a real MeterProvider, which is the
// behaviour a reintroduced "don't export to yourself" branch would break.
func TestNodeStatSamplerExportsOnLeader(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	// The swarm view of this host: manager, and the current leader.
	dc := &statClient{nodes: []runtime.Node{
		{ID: "self", Hostname: "self", Role: "manager", IsLeader: true},
		{ID: "peer", Hostname: "nxt-sw-4-m", Role: "manager"},
	}}
	s := nodestat.NewSampler(writeProcTree(t), "self", "manager")
	ctx := context.Background()
	nodeStatTick(ctx, dc, s, zerolog.Nop())

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect exported metrics: %v", err)
	}
	load, host := findGauge(rm, nodestat.MetricLoad1)
	if !load {
		t.Fatal("a leader daemon exported no pmcluster.node.load.1: its sample was skipped")
	}
	if host != "self" {
		t.Fatalf("exported host.name = %q, want %q", host, "self")
	}
	if found, _ := findGauge(rm, nodestat.MetricProcessRSS); !found {
		t.Fatal("a leader daemon exported no process series: its top list was skipped")
	}
}

// findGauge looks for one metric in a collected batch and returns whether it
// has at least one datapoint plus that datapoint's host.name.
func findGauge(rm metricdata.ResourceMetrics, name string) (bool, string) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[float64])
			if !ok || len(g.DataPoints) == 0 {
				return false, ""
			}
			host, _ := g.DataPoints[0].Attributes.Value("host.name")
			return true, host.AsString()
		}
	}
	return false, ""
}
