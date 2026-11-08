//go:build e2e

// Node-stats e2e: one fixture file shaped exactly like the collector's `file`
// exporter output goes through the REAL reader, the REAL store and the REAL
// console renderer, and the overview table is asserted on the produced HTML.
//
// It deliberately needs no Docker and no Swarm: the panel's whole reason for
// existing is that it must work when the telemetry pipeline and the swarm API
// are the things that are broken. requireSwarm would skip it on the swarm-e2e
// runner for no reason.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/api"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodestat"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/controllers"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
	uistore "github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/views"
)

// nsProc is one process in the fixture.
type nsProc struct {
	PID  int
	Name string
	RSS  float64
	CPU  float64
}

// nsLine renders one collector-output line: an OTLP/JSON
// ExportMetricsServiceRequest with a single resource, exactly as the `file`
// exporter writes it.
func nsLine(t *testing.T, host, role string, ts int64, load float64, memTotal, memAvail int64, dockerUp bool, procs []nsProc) string {
	t.Helper()
	str := func(s string) map[string]any { return map[string]any{"stringValue": s} }
	attrs := func(kv map[string]any) []map[string]any {
		out := make([]map[string]any, 0, len(kv))
		for k, v := range kv {
			out = append(out, map[string]any{"key": k, "value": v})
		}
		return out
	}
	dp := func(v float64) []map[string]any {
		return []map[string]any{{
			"asDouble":     v,
			"timeUnixNano": fmt.Sprintf("%d", ts*int64(time.Second)),
			"attributes":   attrs(map[string]any{"host.name": str(host)}),
		}}
	}
	metric := func(name string, v float64) map[string]any {
		return map[string]any{"name": name, "gauge": map[string]any{"dataPoints": dp(v)}}
	}

	metrics := []map[string]any{
		metric(nodestat.MetricLoad1, load),
		metric(nodestat.MetricLoad5, load),
		metric(nodestat.MetricLoad15, load),
		metric(nodestat.MetricMemTotalMB, float64(memTotal)),
		metric(nodestat.MetricMemUsedMB, float64(memTotal-memAvail)),
		metric(nodestat.MetricMemAvailMB, float64(memAvail)),
		metric(nodestat.MetricCores, 1),
	}
	up := 0.0
	if dockerUp {
		up = 1
	}
	metrics = append(metrics, metric(nodestat.MetricDockerUp, up))
	for _, p := range procs {
		pa := attrs(map[string]any{
			"host.name":    str(host),
			"process.pid":  map[string]any{"intValue": fmt.Sprintf("%d", p.PID)},
			"process.name": str(p.Name),
		})
		for _, pair := range []struct {
			name string
			v    float64
		}{{nodestat.MetricProcessRSS, p.RSS}, {nodestat.MetricProcessCPU, p.CPU}} {
			metrics = append(metrics, map[string]any{
				"name": pair.name,
				"gauge": map[string]any{"dataPoints": []map[string]any{{
					"asDouble":     pair.v,
					"timeUnixNano": fmt.Sprintf("%d", ts*int64(time.Second)),
					"attributes":   pa,
				}}},
			})
		}
	}

	doc := map[string]any{"resourceMetrics": []map[string]any{{
		"resource": map[string]any{"attributes": attrs(map[string]any{
			"host.name": str(host), "node.role": str(role),
		})},
		"scopeMetrics": []map[string]any{{"metrics": metrics}},
	}}}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal fixture line: %v", err)
	}
	return string(b)
}

// TestNodeStatsPanelEndToEnd is the whole panel: fixture JSONL → reader → store
// row → API JSON → rendered console table.
func TestNodeStatsPanelEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nodestat.jsonl")
	now := time.Now().Unix()

	// Seven processes on nxt-sw-4-m so the 5-entry cap is observable: the two
	// smallest by RSS are outside the top five, and one of them has the highest
	// CPU so it must still appear in the CPU list.
	procs := []nsProc{
		{101, "pmcluster", 902, 3},
		{102, "dockerd", 210, 5},
		{103, "containerd", 90, 2},
		{104, "chronyd", 20, 1},
		{105, "journald", 15, 1},
		{106, "cron", 4, 44},         // high CPU, low RSS
		{107, "tiny-helper", 1, 0.1}, // outside BOTH lists
	}
	line := func(host, role string, ts int64, load float64, total, avail int64, up bool, ps []nsProc) string {
		return nsLine(t, host, role, ts, load, total, avail, up, ps) + "\n"
	}

	first := line("nxt-sw-5-m", "manager", now-40, 0.4, 1839, 1200, true, procs[:2]) +
		line("nxt-sw-4-m", "manager", now-40, 9.96, 1839, 180, true, procs) +
		line("nxt-sw-3-w", "worker", now-600, 0.1, 1839, 1200, true, procs[:1])
	if err := os.WriteFile(path, []byte(first), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// The store the leader writes and the console reads.
	db, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	reader := nodestat.NewReader(path)
	res, err := reader.Read()
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if len(res.Stats) != 3 {
		t.Fatalf("first read = %d nodes, want 3", len(res.Stats))
	}
	if err := db.UpsertNodeStats(ctx, res.Stats, now); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Second tick: a NEW sample for two nodes and one torn line between them.
	// The torn line is what a rotation boundary looks like; it must cost the
	// panel nothing.
	torn := first[:len(first)/3]
	appendFileE2E(t, path, line("nxt-sw-5-m", "manager", now-20, 0.5, 1839, 1100, true, procs[:2])+
		torn+
		line("nxt-sw-4-m", "manager", now-20, 11.2, 1839, 150, true, procs))
	res, err = reader.Read()
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if res.Malformed != 1 {
		t.Fatalf("malformed = %d, want the one torn line", res.Malformed)
	}
	if err := db.UpsertNodeStats(ctx, res.Stats, now); err != nil {
		t.Fatalf("upsert second tick: %v", err)
	}

	// Rotation: the exporter renames the file and starts a fresh one. The
	// reader must rebuild from the tail, pick up the new samples, and keep the
	// silent node's last row.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if err := os.WriteFile(path, []byte(
		line("nxt-sw-5-m", "manager", now-5, 0.6, 1839, 1050, true, procs[:2])+
			line("nxt-sw-4-m", "manager", now-5, 12.0, 1839, 100, false, procs)), 0o600); err != nil {
		t.Fatalf("write post-rotation fixture: %v", err)
	}
	res, err = reader.Read()
	if err != nil {
		t.Fatalf("read after rotation: %v", err)
	}
	if !res.Rebuilt {
		t.Fatal("rotation did not trigger a tail rebuild")
	}
	if err := db.UpsertNodeStats(ctx, res.Stats, now); err != nil {
		t.Fatalf("upsert after rotation: %v", err)
	}

	// Exactly ONE row per node, no matter how many samples crossed the reader.
	rows, err := db.ListNodeStats(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want one per node (3)", len(rows))
	}

	// The newest sample wins, and top-5 is correct.
	byHost := map[string]store.NodeStat{}
	for _, r := range rows {
		byHost[r.Hostname] = r
	}
	fourM := byHost["nxt-sw-4-m"]
	if fourM.Load1 != 12.0 || fourM.DockerUp != 0 {
		t.Fatalf("nxt-sw-4-m = load %v dockerUp %d, want 12.0 / 0 from the post-rotation sample", fourM.Load1, fourM.DockerUp)
	}
	if len(fourM.TopRSS) != 5 {
		t.Fatalf("top rss = %d entries, want 5", len(fourM.TopRSS))
	}
	if fourM.TopRSS[0].Name != "pmcluster" || fourM.TopRSS[0].RSSMB != 902 {
		t.Fatalf("top rss[0] = %+v, want pmcluster at 902 MB", fourM.TopRSS[0])
	}
	if fourM.TopRSS[4].Name != "journald" {
		t.Fatalf("top rss[4] = %+v, want journald (the 5th of 7)", fourM.TopRSS[4])
	}
	if len(fourM.TopCPU) != 5 || fourM.TopCPU[0].Name != "cron" || fourM.TopCPU[0].CPUPct != 44 {
		t.Fatalf("top cpu = %+v, want cron first at 44%%", fourM.TopCPU)
	}
	// A node that stopped reporting kept its last row, aged.
	if byHost["nxt-sw-3-w"].SampleTS != now-600 {
		t.Fatalf("silent node sample_ts = %d, want its last sample", byHost["nxt-sw-3-w"].SampleTS)
	}

	// ---- the rendered console -------------------------------------------------
	handler := api.NodeStatsHandler(db)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/node-stats", handler)
	mux.HandleFunc("/api/me", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":1,"name":"e2e","role":"operator"}`))
	})
	mux.HandleFunc("/api/nodes", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"nodes":[{"hostname":"nxt-sw-5-m","role":"manager","status":"ready","is_leader":true},{"hostname":"nxt-sw-4-m","role":"manager","status":"ready"},{"hostname":"nxt-sw-3-w","role":"worker","status":"ready"}]}`))
	})
	mux.HandleFunc("/api/cluster/info", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"node_name":"nxt-sw-5-m","server_version":"28","os":"linux","arch":"amd64","cpus":2,"memory_bytes":1937240064,"swarm":{"state":"active","control_available":true,"managers":2,"nodes":3}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	uiDB, err := uistore.Open(filepath.Join(dir, "ui"))
	if err != nil {
		t.Fatalf("ui store: %v", err)
	}
	t.Cleanup(func() { _ = uiDB.Close() })
	renderer, err := views.NewRenderer()
	if err != nil {
		t.Fatalf("renderer: %v", err)
	}
	renderer.ShellData = views.ShellBase
	ctrl := &controllers.Controller{
		Store:    uiDB,
		API:      pmapi.New(srv.URL, "e2e", 5*time.Second),
		Views:    renderer,
		EnvAPI:   srv.URL,
		EnvToken: "e2e",
	}
	html := renderFragment(t, ctrl)

	for _, want := range []string{
		"nxt-sw-5-m", "nxt-sw-4-m", "nxt-sw-3-w",
		"12.00",               // live load on 4-m
		"1,739 / 1,839 MB",    // memory used / total on 4-m
		"pmcluster", "902 MB", // top by memory
		"44.0%", // top by cpu
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered overview is missing %q", want)
		}
	}

	// The stale node is greyed AND labelled; the live ones are neither.
	if !strings.Contains(html, `class="is-stale"`) {
		t.Error("the node that stopped reporting is not greyed")
	}
	if !strings.Contains(html, "stale") {
		t.Error("the node that stopped reporting is not labelled stale")
	}
	if n := strings.Count(html, `class="is-stale"`); n != 1 {
		t.Errorf("is-stale appears %d times, want exactly 1 (nxt-sw-3-w)", n)
	}
	// The process outside both top-five lists must not be rendered.
	if strings.Contains(html, "tiny-helper") {
		t.Error("a process outside the top 5 reached the table")
	}
	// A torn line must not have produced a bogus zero row.
	if strings.Contains(html, "0 / 0 MB") {
		t.Error("a malformed line produced an empty node row")
	}
}

// renderFragment drives the overview fragment through the real renderer and
// returns the HTML.
func renderFragment(t *testing.T, ctrl *controllers.Controller) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/web/overview", nil)
	ctx, _ := ginTestContext(rec, req)
	controllers.Overview{Controller: ctrl}.Fragment(ctx)
	return rec.Body.String()
}

func appendFileE2E(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("append: %v", err)
	}
}

// ginTestContext builds the *gin.Context a handler needs without a router.
func ginTestContext(w http.ResponseWriter, r *http.Request) (*gin.Context, bool) {
	c, _ := gin.CreateTestContext(w)
	c.Request = r
	return c, true
}
