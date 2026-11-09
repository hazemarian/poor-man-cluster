package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// leaderMoveOptions is the election timing the handler uses. Production takes
// the defaults; tests shorten them so a handler test does not wait out the real
// election window.
var leaderMoveOptions = cluster.LeaderMoveOptions{}

// ClusterLeaderHandler answers POST /api/cluster/leader/{hostname}: it forces a
// Swarm election onto that manager and reports where leadership actually ended
// up. The CLI's `cluster leader` uses it in remote mode; it is a testing
// affordance for failover, not something the daemon calls on its own.
//
// Refusals (the target is not a manager, the move would leave fewer than three
// managers, the swarm has no quorum) are 409: they are answers, not failures.
// A Docker or transport failure is a 502.
func ClusterLeaderHandler(d runtime.Client) http.HandlerFunc {
	writeErr := func(w http.ResponseWriter, status int, msg string) {
		writeJSON(w, status, map[string]any{"error": msg})
	}
	return func(w http.ResponseWriter, r *http.Request) {
		hostname := chi.URLParam(r, "hostname")
		if hostname == "" {
			writeErr(w, http.StatusBadRequest, "hostname required")
			return
		}
		res, err := cluster.MoveLeadership(r.Context(), d, hostname, leaderMoveOptions)
		if err != nil {
			var refusal *cluster.LeaderMoveRefusal
			if errors.As(err, &refusal) {
				writeErr(w, http.StatusConflict, refusal.Error())
				return
			}
			writeErr(w, http.StatusBadGateway, fmt.Sprintf("move leadership: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"leader":    res.Leader,
			"previous":  res.From,
			"elections": res.Elections,
			"changed":   res.Changed(),
		})
	}
}
