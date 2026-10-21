package controllers

import (
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

// TestObservabilityCacheSharesOneProbePerFragment pins what the cache is for: an
// observability page draws a shell plus several panels, and every one of them
// used to pay the /api/me probe and the ~1.8s backup discovery listing. A cache
// that answered "miss" every time would leave the page exactly as slow.
func TestObservabilityCacheSharesOneProbePerFragment(t *testing.T) {
	c := NewObservabilityCache()

	if _, fresh := c.MeAuthorized(); fresh {
		t.Fatal("a fresh cache must not claim a cached probe")
	}
	c.RecordMe(true)
	if ok, fresh := c.MeAuthorized(); !fresh || !ok {
		t.Fatalf("recorded probe not served from cache (fresh=%v ok=%v)", fresh, ok)
	}

	// A denial is cached too — otherwise a scoped token re-probes on every panel.
	c.RecordMe(false)
	if ok, fresh := c.MeAuthorized(); !fresh || ok {
		t.Fatalf("recorded denial not served from cache (fresh=%v ok=%v)", fresh, ok)
	}

	// Expiry: the entry must go stale, or a revoked token would keep working.
	c.mu.Lock()
	c.meUntil = time.Now().Add(-time.Second)
	c.mu.Unlock()
	if _, fresh := c.MeAuthorized(); fresh {
		t.Fatal("an expired probe must not be served from cache")
	}
}

// TestObservabilityCacheBackups pins the second half: the listing is served while
// fresh, refreshed when stale, and an empty listing never replaces a real one.
func TestObservabilityCacheBackups(t *testing.T) {
	c := NewObservabilityCache()

	if _, ok := c.Backups(); ok {
		t.Fatal("empty cache must miss")
	}
	want := []pmapi.Backup{{ID: 1}}
	c.RecordBackups(want)
	got, ok := c.Backups()
	if !ok || len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("recorded listing not served (ok=%v got=%v)", ok, got)
	}

	c.mu.Lock()
	c.backupAt = time.Now().Add(-time.Minute)
	c.mu.Unlock()
	if _, ok := c.Backups(); ok {
		t.Fatal("a stale listing must not be served")
	}

	// nil must not clear a good entry (a failed read should not blank the panel).
	c.RecordBackups(want)
	c.RecordBackups(nil)
	if _, ok := c.Backups(); !ok {
		t.Fatal("a nil listing cleared a valid cache entry")
	}
}

// TestObservabilityCacheNilSafe: the controller may be built without a cache in
// tests and non-console call paths; a nil must behave like "no cache", not panic.
func TestObservabilityCacheNilSafe(t *testing.T) {
	var c *ObservabilityCache
	if _, fresh := c.MeAuthorized(); fresh {
		t.Fatal("nil cache reported a fresh probe")
	}
	if _, ok := c.Backups(); ok {
		t.Fatal("nil cache reported a hit")
	}
	c.RecordMe(true)
	c.RecordBackups([]pmapi.Backup{{ID: 2}})
}
