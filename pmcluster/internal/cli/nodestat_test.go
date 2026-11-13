package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodereport"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodestat"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
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

// TestNodeStatTickReportsAndDoesNotPublish: the tick's job is to SEND, not to export.
//
// The leader is the only node with a collector, so the collector's copy of a worker's
// numbers can only arrive through the report handler. Publishing in the tick as well
// would double every point on the leader's own series — and publishing on a worker is a
// no-op that looks like success while nothing reaches the pipeline.
//
// This pins both halves: one POST with this node's sample, and zero metrics recorded
// locally by the sampler.
func TestNodeStatTickReportsAndDoesNotPublish(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	dc := &statClient{nodes: []runtime.Node{
		{ID: "self", Hostname: "self", Role: "manager", IsLeader: true},
	}}
	s := nodestat.NewSampler(writeProcTree(t), "self", "manager")
	sender := &fakeReportSender{}
	ctx := context.Background()
	nodeStatTickWith(ctx, dc, s, zerolog.Nop(), sender)

	if len(sender.posts) != 1 {
		t.Fatalf("reports sent = %d, want exactly 1", len(sender.posts))
	}
	if sender.posts[0] != nodereport.Path {
		t.Fatalf("posted to %q, want the internal report path %q", sender.posts[0], nodereport.Path)
	}
	if !strings.Contains(string(sender.bodies[0]), `"hostname":"self"`) {
		t.Fatalf("the report does not name this node: %s", sender.bodies[0])
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if load, _ := findGauge(rm, nodestat.MetricLoad1); load {
		t.Fatal("the sampler published metrics directly: the report handler is the only emitter, or the leader's series doubles")
	}
}

// fakeReportSender records what the tick would put on the wire.
type fakeReportSender struct {
	posts  []string
	bodies [][]byte
}

func (f *fakeReportSender) PostJSON(_ context.Context, path string, body []byte) error {
	f.posts = append(f.posts, path)
	f.bodies = append(f.bodies, body)
	return nil
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
