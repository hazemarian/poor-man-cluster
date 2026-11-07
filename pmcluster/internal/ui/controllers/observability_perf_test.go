package controllers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

// overlapSource records how many Logs calls are inside the fake at once, so a
// sequential implementation fails the test instead of merely being slower.
type overlapSource struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	delay    time.Duration
	results  map[string]observability.Result
	failOn   string
}

func (s *overlapSource) Logs(_ context.Context, q observability.LogQuery) (observability.Result, error) {
	s.mu.Lock()
	s.inFlight++
	if s.inFlight > s.peak {
		s.peak = s.inFlight
	}
	s.mu.Unlock()

	time.Sleep(s.delay)

	s.mu.Lock()
	s.inFlight--
	s.mu.Unlock()

	if s.failOn != "" && q.Severity == s.failOn {
		return observability.Result{}, errors.New("boom")
	}
	return s.results[q.Severity], nil
}

func row(ts, svc, body string) observability.Row {
	return observability.Row{"p_timestamp": ts, "service.name": svc, "body": body}
}

// TestRecentForInvestigationOverlapsItsTwoQueries is the assertion that the
// parallel rewrite actually overlaps: the fake records the peak number of
// simultaneous calls, so a sequential implementation reports 1 and fails.
func TestRecentForInvestigationOverlapsItsTwoQueries(t *testing.T) {
	src := &overlapSource{
		delay: 60 * time.Millisecond,
		results: map[string]observability.Result{
			"error": {Status: observability.StatusAvailable, Rows: []observability.Row{row("2026-11-07T10:00:00Z", "svc", "boom")}},
			"":      {Status: observability.StatusAvailable, Rows: []observability.Row{row("2026-11-07T10:00:01Z", "svc", "fine")}},
		},
	}

	start := time.Now()
	merged, err := recentForInvestigation(context.Background(), src, "svc", observability.Window{})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src.peak != 2 {
		t.Fatalf("peak concurrent calls = %d, want 2 (queries ran sequentially)", src.peak)
	}
	// Two 60ms calls overlapped must finish well inside two sequential calls.
	if elapsed > 100*time.Millisecond {
		t.Fatalf("elapsed %v looks sequential (2 x %v)", elapsed, src.delay)
	}
	// Order is unchanged: the error rows come first, then any-level.
	if len(merged.Rows) != 2 || merged.Rows[0]["body"] != "boom" || merged.Rows[1]["body"] != "fine" {
		t.Fatalf("merge order changed: %+v", merged.Rows)
	}
}

func TestRecentForInvestigationDedupesAndSurfacesFailures(t *testing.T) {
	dup := row("2026-11-07T10:00:00Z", "svc", "same line")
	src := &overlapSource{results: map[string]observability.Result{
		"error": {Status: observability.StatusAvailable, Rows: []observability.Row{dup}},
		"":      {Status: observability.StatusAvailable, Rows: []observability.Row{dup, row("2026-11-07T10:00:05Z", "svc", "other")}},
	}}
	merged, err := recentForInvestigation(context.Background(), src, "svc", observability.Window{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(merged.Rows) != 2 {
		t.Fatalf("rows = %d, want 2 after de-duplication: %+v", len(merged.Rows), merged.Rows)
	}

	// One failing call must fail the whole investigation: a success must not
	// mask a failure, or the operator silently sees half the evidence.
	broken := &overlapSource{
		failOn: "error",
		results: map[string]observability.Result{
			"": {Status: observability.StatusAvailable, Rows: []observability.Row{row("2026-11-07T10:00:00Z", "svc", "fine")}},
		},
	}
	if _, err := recentForInvestigation(context.Background(), broken, "svc", observability.Window{}); err == nil {
		t.Fatal("a failing severity call must fail the investigation")
	}
}

func TestEvidenceCacheServesThenExpires(t *testing.T) {
	c := NewEvidenceCache()
	key := evidenceCacheKey{service: "svc", rangeVal: "1h", severity: "error"}
	want := observability.Result{Status: observability.StatusAvailable, Rows: []observability.Row{row("t", "svc", "b")}}

	c.Put(key, want)
	got, ok := c.Get(key)
	if !ok || len(got.Rows) != 1 {
		t.Fatalf("fresh entry not served: ok=%v rows=%d", ok, len(got.Rows))
	}

	// Backdate past the TTL: the entry must stop being served.
	c.mu.Lock()
	e := c.entries[key]
	e.at = time.Now().Add(-evidenceCacheTTL - time.Second)
	c.entries[key] = e
	c.mu.Unlock()

	if _, ok := c.Get(key); ok {
		t.Fatal("expired entry still served")
	}
}

func TestEvidenceCacheKeyIsPerFilter(t *testing.T) {
	c := NewEvidenceCache()
	base := evidenceCacheKey{service: "svc", rangeVal: "1h", severity: "error", search: "", offset: 0}
	c.Put(base, observability.Result{Status: observability.StatusAvailable})

	for _, different := range []evidenceCacheKey{
		{service: "other", rangeVal: "1h", severity: "error"},
		{service: "svc", rangeVal: "24h", severity: "error"},
		{service: "svc", rangeVal: "1h", severity: "warn"},
		{service: "svc", rangeVal: "1h", severity: "error", search: "needle"},
		{service: "svc", rangeVal: "1h", severity: "error", offset: 50},
	} {
		if _, ok := c.Get(different); ok {
			t.Errorf("cache served a different filter combination: %+v", different)
		}
	}
}

func TestEvidenceCacheIsBounded(t *testing.T) {
	c := NewEvidenceCache()
	for i := 0; i < evidenceCacheMax+10; i++ {
		c.Put(evidenceCacheKey{service: "svc", rangeVal: "1h", offset: i}, observability.Result{Status: observability.StatusAvailable})
	}
	c.mu.Lock()
	n := len(c.entries)
	_, hasFirst := c.entries[evidenceCacheKey{service: "svc", rangeVal: "1h", offset: 0}]
	c.mu.Unlock()

	if n > evidenceCacheMax {
		t.Fatalf("cache holds %d entries, cap is %d", n, evidenceCacheMax)
	}
	if hasFirst {
		t.Error("oldest entry survived eviction")
	}
}

// TestEvidenceCacheNilIsSafe documents the nil-receiver path the controller
// relies on when it is built without a cache (tests, one-shot commands).
func TestEvidenceCacheNilIsSafe(t *testing.T) {
	var c *EvidenceCache
	if _, ok := c.Get(evidenceCacheKey{}); ok {
		t.Fatal("nil cache reported a hit")
	}
	c.Put(evidenceCacheKey{}, observability.Result{}) // must not panic
}
