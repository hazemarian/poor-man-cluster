package ci

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// HTTP serves the board to the console as GET /ci/runs.
//
// Read-only, and safe to call often: the Board decides when an answer is a real
// GitHub call and when it is the cache, so a console page that polls this endpoint
// costs nothing it did not already cost.
type HTTP struct {
	Board *Board
}

// Mount registers the route. The caller decides the auth scope (the console reads
// it for any signed-in user; the API needs an operator token like the runners
// surface next to it).
func (h *HTTP) Mount(r chi.Router) {
	r.Get("/ci/runs", h.snapshot)
}

func (h *HTTP) snapshot(w http.ResponseWriter, r *http.Request) {
	if h.Board == nil {
		http.Error(w, "the CI board is not configured on this daemon", http.StatusNotImplemented)
		return
	}
	snap := h.Board.Snapshot(r.Context())
	w.Header().Set("Content-Type", "application/json")
	if snap.Error != "" {
		// The board is degraded, not broken: it still carries the last good rows.
		// 200 with the error inside keeps the panel usable; a 5xx would blank it at
		// exactly the moment an operator is looking for answers.
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(snap)
}
