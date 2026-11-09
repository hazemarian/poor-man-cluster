package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/rs/zerolog"

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
	nodes      []runtime.Node
	nodeErr    error
	labelCalls []string
	restarts   []string
}

func (c *statClient) NodeList(context.Context) ([]runtime.Node, error) {
	return c.nodes, c.nodeErr
}

func (c *statClient) Info(context.Context) (runtime.Info, error) {
	return runtime.Info{Name: "self"}, nil
}

func (c *statClient) SetNodeLabel(_ context.Context, id, key, value string) error {
	c.labelCalls = append(c.labelCalls, id+":"+key+":"+value)
	return nil
}

func (c *statClient) ServiceRestart(_ context.Context, id string) error {
	c.restarts = append(c.restarts, id)
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
	if len(dc.labelCalls) != 1 || dc.labelCalls[0] != "self:"+runtime.LeaderNodeLabel+":true" {
		t.Fatalf("label calls = %v, want one true on self", dc.labelCalls)
	}

	// Already labelled: a restart must not rewrite the node spec.
	dc.nodes[0].Labels = map[string]string{runtime.LeaderNodeLabel: "true"}
	dc.labelCalls = nil
	if syncLeaderLabel(context.Background(), dc, true, zerolog.Nop()) {
		t.Fatal("a second gain reported a change; the collector would restart on every daemon restart")
	}
	if len(dc.labelCalls) != 0 {
		t.Fatalf("label calls = %v, want none when already set", dc.labelCalls)
	}

	// Leadership lost: cleared (an empty value removes the key in the adapter).
	if !syncLeaderLabel(context.Background(), dc, false, zerolog.Nop()) {
		t.Fatal("losing leadership reported no change")
	}
	if len(dc.labelCalls) != 1 || dc.labelCalls[0] != "self:"+runtime.LeaderNodeLabel+":" {
		t.Fatalf("label calls = %v, want one clear on self", dc.labelCalls)
	}

	// Never touches another node while that node carries no label: the label names
	// the leader, not every manager. (A STALE label on another node is cleared —
	// see the next test — because two matches make the collector's constraint
	// ambiguous.)
	for _, c := range dc.labelCalls {
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
	if len(dc.labelCalls) != 1 || dc.labelCalls[0] != "self:"+runtime.LeaderNodeLabel+":true" {
		t.Fatalf("label calls = %v, want the label stamped on self", dc.labelCalls)
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
	if len(dc.labelCalls) != 1 || dc.labelCalls[0] != "old:"+runtime.LeaderNodeLabel+":" {
		t.Fatalf("label calls = %v, want exactly the stale label cleared on old", dc.labelCalls)
	}

	// A follower must not police other nodes: the leader owns that decision. Its
	// own (absent) label is written as a clear; the leader's label is left alone.
	dc2 := &statClient{nodes: []runtime.Node{
		{ID: "self", Hostname: "self", Role: "manager"},
		{ID: "leader", Hostname: "nxt-sw-5-m", Role: "manager", IsLeader: true,
			Labels: map[string]string{runtime.LeaderNodeLabel: "true"}},
	}}
	syncLeaderLabel(context.Background(), dc2, false, zerolog.Nop())
	for _, c := range dc2.labelCalls {
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
	for len(dc.labelCalls) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if len(dc.labelCalls) == 0 {
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
	if len(dc.restarts) != 1 || dc.restarts[0] != "observability_otel-collector" {
		t.Fatalf("restarts = %v, want exactly the collector service", dc.restarts)
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
