package cli

import (
	"net/http"
	"sync/atomic"
)

// adoptionGate answers 503 while the daemon is swapping its control-plane
// store, so a request that depends on control-plane state is never served from
// the revision the node is in the middle of replacing.
//
// The window it covers is the leadership transition itself: the moment Swarm
// elects this node leader, the newest Raft-replicated kit may be newer than the
// local data.db, and adopting it means closing the SQLite connection, renaming
// a new data.db into place and reopening. Answering "not ready, retry" for
// those few tens of milliseconds is the honest trade — the alternative is an
// answer built from pre-move credentials and settings, which is exactly the
// silent rollback this machinery exists to prevent.
//
// /health stays ungated: liveness must answer during an adoption, or Swarm (and
// the console) would see a leader that appears down.
type adoptionGate struct {
	busy atomic.Bool
}

// begin marks the daemon as adopting and returns the function that ends it.
func (g *adoptionGate) begin() func() {
	g.busy.Store(true)
	return func() { g.busy.Store(false) }
}

// adopting reports whether an adoption is in progress.
func (g *adoptionGate) adopting() bool { return g.busy.Load() }

// wrap gates h behind the flag.
func (g *adoptionGate) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.adopting() && r.URL.Path != "/health" {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"adopting the cluster's control plane after a leadership change — retry in a moment"}`))
			return
		}
		h.ServeHTTP(w, r)
	})
}
