package api

import (
	"net/http"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// ClusterInfoHandler answers GET /api/cluster/info with node and engine
// state, or a 503 when Docker is unavailable.
func ClusterInfoHandler(d runtime.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info, err := d.Info(r.Context())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"node_name":      info.Name,
			"server_version": info.ServerVersion,
			"os":             info.OperatingSystem,
			"arch":           info.Architecture,
			"cpus":           info.NCPU,
			"memory_bytes":   info.MemTotal,
			"swarm": map[string]any{
				"state":             info.SwarmLocalNodeState,
				"control_available": info.SwarmControlAvailable,
				"managers":          info.SwarmManagers,
				"nodes":             info.SwarmNodes,
			},
		})
	}
}
