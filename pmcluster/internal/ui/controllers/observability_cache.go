package controllers

import (
	"sync"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

// ObservabilityCache memoises the two daemon reads that dominate an observability
// page load and do not change meaningfully second to second:
//
//   - the /api/me authorization probe (a scope check, not presentation data)
//   - the backup listing, which runs a discovery pass over the archive directory
//     and the store (measured at ~1.8s on the live cluster)
//
// Both were paid by EVERY fragment — the shell, the overview, the logs panel and
// the traces panel — so a page drawing four panels paid them four times. This is
// a console-side cache, not a daemon-side one: the daemon stays the authority, it
// is simply not asked the same question four times in one second.
//
// Deliberately tiny: two values, one TTL, no invalidation API. A backup age that
// is thirty seconds stale is not a correctness problem; a page that takes eight
// seconds is.
type ObservabilityCache struct {
	mu       sync.Mutex
	meOK     bool
	meUntil  time.Time
	backups  []pmapi.Backup
	backupAt time.Time
}

// observabilityCacheTTL is how long a cached read is served. Short enough that a
// fresh backup or a policy change shows up while the operator is still looking,
// long enough that a single page load never repeats the read.
const observabilityCacheTTL = 30 * time.Second

// NewObservabilityCache builds an empty cache.
func NewObservabilityCache() *ObservabilityCache { return &ObservabilityCache{} }

// MeAuthorized reports whether the /api/me probe succeeded recently, so a burst
// of fragments shares one probe instead of making one each.
func (c *ObservabilityCache) MeAuthorized() (ok bool, fresh bool) {
	if c == nil {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.meUntil.IsZero() || time.Now().After(c.meUntil) {
		return false, false
	}
	return c.meOK, true
}

// RecordMe stores the probe outcome.
func (c *ObservabilityCache) RecordMe(ok bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.meOK = ok
	c.meUntil = time.Now().Add(observabilityCacheTTL)
}

// Backups returns a cached listing when one is fresh.
func (c *ObservabilityCache) Backups() ([]pmapi.Backup, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.backups == nil || time.Since(c.backupAt) > observabilityCacheTTL {
		return nil, false
	}
	return c.backups, true
}

// RecordBackups stores a listing for the TTL.
func (c *ObservabilityCache) RecordBackups(v []pmapi.Backup) {
	if c == nil || v == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.backups = v
	c.backupAt = time.Now()
}
