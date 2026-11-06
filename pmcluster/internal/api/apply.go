package api

import (
	"net/http"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// ApplyRecordHandler reports the last platform apply: what it changed, and —
// the part that used to go nowhere — what it could not do. Every degraded step
// in `cluster update` is a warning written to the run output; the daemon's
// reconcile loop runs unattended, so those warnings only ever reached journald.
// This is the surface that makes them visible to a human.
func ApplyRecordHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if st == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no cluster store"})
			return
		}
		rec := cluster.LoadApplyRecord(r.Context(), st)
		if rec == nil {
			writeJSON(w, http.StatusOK, map[string]any{"present": false})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"present":       true,
			"at":            rec.At,
			"version":       rec.Version,
			"source":        rec.Source,
			"stacks":        rec.Stacks,
			"configs":       rec.Configs,
			"warnings":      rec.Warnings,
			"warning_count": len(rec.Warnings),
			"line":          rec.Line(),
		})
	}
}
