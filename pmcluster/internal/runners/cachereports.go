package runners

import (
	"sync"
	"time"
)

// CacheReports is where a node's cache measurements live ON THE LEADER.
//
// The problem it solves is structural: the console is served by the leader, and the
// volumes belong to the node that runs the runner. A worker's API is loopback-only, so
// the leader cannot ask it anything — and `docker system df` only answers for the daemon
// it runs against. Without this channel the check exists and the page that needs it can
// never show it: measured on a live fleet, the leader's own runner rows carried sizes and
// the working worker's row carried nothing.
//
// So the node reports what it measured when it polls for its assignment, and the leader
// keeps the last report per node. It is deliberately in MEMORY and not in the store: it
// is a measurement with a timestamp, it is cheap to take again, and a leader that just
// took over should say "not measured yet" rather than serve numbers from a leader that no
// longer exists.
type CacheReports struct {
	mu     sync.RWMutex
	byNode map[string]CacheReport
	// Now is the clock (tests).
	Now func() time.Time
}

// CacheReport is one node's measurement.
type CacheReport struct {
	Caches []CacheUsage `json:"caches"`
	// At is when the node took the measurement. A report older than a few minutes is
	// stale — the node polls every minute, so silence means the node stopped talking,
	// not that the caches shrank.
	At time.Time `json:"at"`
}

// NewCacheReports returns an empty set of reports.
func NewCacheReports() *CacheReports { return &CacheReports{byNode: map[string]CacheReport{}} }

// Put records a node's report. The node name is normalised: a report for "NXT-SW-6-W"
// and one for "nxt-sw-6-w" are the same node, and a lookup that missed would silently
// show "not measured" forever.
func (c *CacheReports) Put(node string, rep CacheReport) {
	if c == nil || node == "" {
		return
	}
	if rep.At.IsZero() {
		rep.At = c.now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byNode == nil {
		c.byNode = map[string]CacheReport{}
	}
	c.byNode[normaliseNode(node)] = rep
}

// Get returns the last report for a node, and whether it is still recent enough to show.
//
// A stale report is returned with ok=false rather than as fresh data: the sizes may be
// hours old after the node disappeared, and a page that shows them as current is worse
// than one that admits it does not know.
func (c *CacheReports) Get(node string) (CacheReport, bool) {
	if c == nil {
		return CacheReport{}, false
	}
	c.mu.RLock()
	rep, ok := c.byNode[normaliseNode(node)]
	c.mu.RUnlock()
	if !ok {
		return CacheReport{}, false
	}
	if c.now().Sub(rep.At) > CacheReportTTL {
		return rep, false
	}
	return rep, true
}

// CacheReportTTL is how long a report stays believable. The worker polls every minute and
// measures every ten, so anything older than a few polls means the node stopped talking.
const CacheReportTTL = 15 * time.Minute

func (c *CacheReports) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func normaliseNode(node string) string {
	lower := make([]byte, 0, len(node))
	for i := 0; i < len(node); i++ {
		ch := node[i]
		if ch >= 'A' && ch <= 'Z' {
			ch += 'a' - 'A'
		}
		lower = append(lower, ch)
	}
	return string(lower)
}
