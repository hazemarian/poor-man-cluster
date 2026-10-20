package controllers

import (
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

func TestWaterfallNestsOrdersAndMarksErrors(t *testing.T) {
	rows := []observability.Row{
		{"span_id": "c", "parent_id": "b", "name": "db", "start_ns": 1.7e18 + 40e6, "duration_ns": 10e6, "status": 0.0},
		{"span_id": "a", "parent_id": "", "name": "root", "start_ns": 1.7e18, "duration_ns": 100e6, "status": 0.0},
		{"span_id": "b", "parent_id": "a", "name": "pay", "start_ns": 1.7e18 + 20e6, "duration_ns": 60e6, "status": 2.0, "message": "boom"},
	}
	spans, total, errs := buildWaterfall(rows)
	if len(spans) != 3 || spans[0].Name != "root" || spans[1].Name != "pay" || spans[2].Name != "db" {
		t.Fatalf("order: %+v", spans)
	}
	if spans[0].Indent != 0 || spans[1].Indent != obsTraceIndentPx || spans[2].Indent != 2*obsTraceIndentPx {
		t.Errorf("indent: %+v", spans)
	}
	if errs != 1 || !spans[1].Error || spans[0].Error {
		t.Errorf("errors: %d %+v", errs, spans)
	}
	if total != "100.0 ms" || spans[0].Left != 0 || spans[0].Width != 100 {
		t.Errorf("extent: %q %+v", total, spans[0])
	}
	if spans[1].Left < 19.9 || spans[1].Left > 20.1 || spans[1].Width < 59.9 || spans[1].Width > 60.1 {
		t.Errorf("position: %+v", spans[1])
	}
}

func TestWaterfallShowsOrphansAndCyclesWithoutLoopingOrDropping(t *testing.T) {
	rows := []observability.Row{
		{"span_id": "x", "parent_id": "missing", "name": "orphan", "start_ns": 1e18, "duration_ns": 1e6},
		{"span_id": "p", "parent_id": "q", "name": "cycle-a", "start_ns": 1e18, "duration_ns": 1e6},
		{"span_id": "q", "parent_id": "p", "name": "cycle-b", "start_ns": 1e18, "duration_ns": 1e6},
		{"span_id": "s", "parent_id": "s", "name": "self", "start_ns": 1e18, "duration_ns": 1e6},
	}
	spans, _, _ := buildWaterfall(rows)
	if len(spans) != 4 {
		t.Fatalf("dropped or duplicated spans: %+v", spans)
	}
}

func TestWaterfallZeroExtentAndDeepIndentAreBounded(t *testing.T) {
	var rows []observability.Row
	parent := ""
	for i := 0; i < 40; i++ {
		id := string(rune('a'+i%26)) + string(rune('A'+i/26))
		rows = append(rows, observability.Row{"span_id": id, "parent_id": parent, "name": id, "start_ns": 1e18, "duration_ns": 0.0})
		parent = id
	}
	spans, total, _ := buildWaterfall(rows)
	if len(spans) != 40 || total != "0 ns" {
		t.Fatalf("%d %q", len(spans), total)
	}
	for _, s := range spans {
		if s.Indent > obsTraceMaxDepthPx || s.Width <= 0 || s.Left+s.Width > 100.0001 {
			t.Fatalf("unbounded: %+v", s)
		}
	}
}

func TestFmtDuration(t *testing.T) {
	for in, want := range map[float64]string{500: "500 ns", 2500: "2 µs", 1.5e6: "1.5 ms", 2.5e9: "2.50 s"} {
		if got := fmtDuration(in); got != want {
			t.Errorf("%v: %q != %q", in, got, want)
		}
	}
}

// The trace service filter reaches a SQL literal, so only the daemon's service
// grammar may pass. Quotes, spaces and extra path segments are refused, and
// "stack/service" maps to the backend name the daemon filter uses.
func TestTraceServiceFilter(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"", "", true},
		{"  ", "", true},
		{"web", "web", true},
		{"demo/web", "demo_web", true},
		{"demo_web", "demo_web", true},
		{"web' OR '1'='1", "", false},
		{"web'--", "", false},
		{"a/b/c", "", false},
		{"demo/", "", false},
		{"/web", "", false},
		{"web worker", "", false},
	}
	for _, tc := range cases {
		got, ok := traceServiceFilter(tc.in)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("traceServiceFilter(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}
