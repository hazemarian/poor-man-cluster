package runners

import (
	"testing"
	"time"
)

// TestCacheReportsArePerNodeAndExpire covers the two ways this map can lie: mixing up two
// nodes (which would show one machine's disk usage for another) and serving an old
// measurement as current.
func TestCacheReportsArePerNodeAndExpire(t *testing.T) {
	now := time.Unix(1700000000, 0)
	r := NewCacheReports()
	r.Now = func() time.Time { return now }

	r.Put("nxt-sw-6-w", CacheReport{Caches: []CacheUsage{{Name: "go", Bytes: 100}}})
	if rep, ok := r.Get("nxt-sw-6-w"); !ok || rep.Caches[0].Bytes != 100 {
		t.Fatalf("the report was not stored: %+v ok=%v", rep, ok)
	}
	// Node names are normalised: a report for one case and a lookup in another are the
	// same node, and a mismatch here would mean "not measured" forever.
	if _, ok := r.Get("NXT-SW-6-W"); !ok {
		t.Fatal("a report was not found under a different case of the same hostname")
	}
	// A node that never reported must not inherit another's numbers.
	if _, ok := r.Get("nxt-sw-4-m"); ok {
		t.Fatal("a node that never reported returned someone else's report")
	}

	// Silence means the node stopped talking — the sizes may be hours old, and showing
	// them as current is worse than admitting we do not know.
	now = now.Add(CacheReportTTL + time.Minute)
	if rep, ok := r.Get("nxt-sw-6-w"); ok {
		t.Fatalf("a stale report was served as fresh: %+v", rep)
	}
}

func TestCacheReportsNilIsSafe(t *testing.T) {
	var r *CacheReports
	r.Put("x", CacheReport{})
	if _, ok := r.Get("x"); ok {
		t.Fatal("a nil sink reported a value")
	}
}

// TestStaleReportKeepsItsDataButSaysSo: Get returns the bytes with ok=false, so a caller
// that wants to say "last seen 2 hours ago, 6 GB" can, and one that wants only current
// numbers does not get them.
func TestStaleReportKeepsItsDataButSaysSo(t *testing.T) {
	now := time.Unix(1700000000, 0)
	r := NewCacheReports()
	r.Now = func() time.Time { return now }
	r.Put("n", CacheReport{Caches: []CacheUsage{{Name: "go", Bytes: 5}}})
	now = now.Add(2 * CacheReportTTL)
	rep, ok := r.Get("n")
	if ok {
		t.Fatal("a stale report was marked fresh")
	}
	if len(rep.Caches) == 0 || rep.Caches[0].Bytes != 5 {
		t.Fatalf("the stale data was discarded instead of returned with ok=false: %+v", rep)
	}
}
