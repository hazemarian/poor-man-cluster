package api

import (
	"net/http"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/runtime"
)

// NodesHandler returns the list of swarm nodes (one row per `docker node ls`
// entry). Read-only — pmcluster doesn't promote/demote/drain via the API.
func NodesHandler(d runtime.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		nodes, err := d.NodeList(r.Context())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		out := make([]map[string]any, 0, len(nodes))
		for _, n := range nodes {
			out = append(out, map[string]any{
				"id":             n.ID,
				"hostname":       n.Hostname,
				"role":           n.Role,
				"availability":   n.Availability,
				"status":         n.Status,
				"is_leader":      n.IsLeader,
				"engine_version": n.EngineVersion,
				"address":        n.Address,
				"created_at":     n.CreatedAt,
				"updated_at":     n.UpdatedAt,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"nodes": out})
	}
}
