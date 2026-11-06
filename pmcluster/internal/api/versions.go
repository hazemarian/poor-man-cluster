package api

import (
	"net/http"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodever"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// VersionsHandler reports the binary version every manager published, compared
// to the version of the daemon answering this request.
func VersionsHandler(d runtime.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no docker client"})
			return
		}
		skew, err := nodever.Read(r.Context(), d, buildinfo.Version)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"reference": skew.Reference,
			"nodes":     skew.Nodes,
			"stale":     skew.Stale,
			"ok":        skew.OK(),
		})
	}
}
