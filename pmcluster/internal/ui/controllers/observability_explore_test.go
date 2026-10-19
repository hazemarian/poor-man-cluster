package controllers

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

func TestBuildChartDrawsGapsNotZeros(t *testing.T) {
	end := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	w := observability.Window{Start: end.Add(-time.Hour), End: end}
	t0 := w.Start.Unix()
	// Two points, a 20 minute hole, then one more: the line must break.
	c := buildChart(map[int64]float64{t0: 10, t0 + 60: 20, t0 + 1260: 30}, w, time.Minute, 100, "%")
	if !c.Known || strings.Count(c.Path, "M") != 2 {
		t.Fatalf("expected two segments: %q", c.Path)
	}
	if c.LastText != "30.0%" || c.PeakText != "30.0%" {
		t.Fatalf("text: %+v", c)
	}
	if empty := buildChart(nil, w, time.Minute, 100, "%"); empty.Known || empty.Path != "" || empty.LastText != "" {
		t.Fatalf("no points must stay unknown: %+v", empty)
	}
}

func TestGroupStacksOrdersUnhealthyFirstAndPages(t *testing.T) {
	var rows []observabilityServiceRow
	for i := 0; i < 20; i++ {
		stack := fmt.Sprintf("stack%02d", i)
		rows = append(rows, observabilityServiceRow{Stack: stack, Service: "web", Path: stack + "/web", Replicas: 1, Desired: 1, DesiredKnown: true, Ready: true})
	}
	rows = append(rows, observabilityServiceRow{Stack: "zz-bad", Service: "db", Path: "zz-bad/db", Replicas: 0, Desired: 1, Deficit: 1, DesiredKnown: true})
	stacks, pager := groupStacks(rows, "", 1)
	if len(stacks) != obsStacksPerPage || stacks[0].Name != "zz-bad" || !stacks[0].Unhealthy {
		t.Fatalf("unhealthy stack not first: %+v", stacks[0])
	}
	if pager.Pages != 3 || !pager.HasNext || pager.HasPrev {
		t.Fatalf("pager: %+v", pager)
	}
	if _, p := groupStacks(rows, "", 99); p.Page != 3 {
		t.Fatalf("page not clamped: %+v", p)
	}
	found, _ := groupStacks(rows, "ZZ-BAD/", 1)
	if len(found) != 1 || found[0].Name != "zz-bad" {
		t.Fatalf("search: %+v", found)
	}
}

func TestProjectServiceMetricsLabelsAndCaps(t *testing.T) {
	var rows []observability.Row
	for i := 0; i < obsTopServices+5; i++ {
		rows = append(rows, observability.Row{"stack": "s", "service": fmt.Sprintf("svc%02d", i), "metric": observability.MetricContainerMemoryPercent, "avg": float64(i), "max": float64(i)})
	}
	rows = append(rows, observability.Row{"container": "orphan.1.abc", "metric": observability.MetricContainerCPU, "avg": 1.0, "max": 2.0})
	got := projectServiceMetrics(rows)
	if len(got) != obsTopServices || got[0].Label != "s/svc19" || got[0].MemPct != 19 {
		t.Fatalf("%+v", got[0])
	}
}

func TestProjectServiceMetricsKeepsReplicaRowsDistinct(t *testing.T) {
	rows := []observability.Row{
		{"stack": "demo", "service": "demo_web", "container": "demo_web.1.high", "metric": observability.MetricContainerMemoryPercent, "avg": 80.0, "max": 95.0},
		{"stack": "demo", "service": "demo_web", "container": "demo_web.2.low", "metric": observability.MetricContainerMemoryPercent, "avg": 10.0, "max": 15.0},
	}

	got := projectServiceMetrics(rows)
	if len(got) != 2 {
		t.Fatalf("replica rows were collapsed: %+v", got)
	}
	if got[0].Label != "demo/web/demo_web.1.high" || got[0].MemMax != 95 {
		t.Fatalf("high replica was projected incorrectly: %+v", got[0])
	}
	if got[1].Label != "demo/web/demo_web.2.low" || got[1].MemMax != 15 {
		t.Fatalf("low replica was projected incorrectly: %+v", got[1])
	}
	if got := serviceMetricLabel(observability.Row{"container": "orphan.1.abc"}); got != "orphan.1.abc" {
		t.Fatalf("container-only label was duplicated: %q", got)
	}
}
