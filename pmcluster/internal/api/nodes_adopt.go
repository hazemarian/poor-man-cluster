package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// NodeStorageAdoptHandler requests (POST) or cancels (DELETE) the adoption of a node
// that already holds stack data.
//
// Adoption is not promotion: it drains the node, copies its data onto a snapshot-capable
// volume, verifies the copy and switches the mount. That stops the node's stacks for the
// duration, which is why it is a separate, explicit request with its own confirm in the
// console — and why it is never implied by adding a hostname to storage_nodes.
//
// The work itself happens on the node (see convergeStorage): copying local data is
// exactly the kind of thing only that machine can do.
func NodeStorageAdoptHandler(d runtime.Client, store interface {
	SettingDefault(ctx context.Context, key, def string) string
	SetSetting(ctx context.Context, key, value string) error
}) http.HandlerFunc {
	writeErr := func(w http.ResponseWriter, status int, msg string) {
		writeJSON(w, status, map[string]any{"error": msg})
	}
	return func(w http.ResponseWriter, r *http.Request) {
		hostname := chi.URLParam(r, "hostname")
		if hostname == "" || store == nil {
			writeErr(w, http.StatusBadRequest, "hostname required")
			return
		}
		nodes, err := d.NodeList(r.Context())
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		var found *runtime.Node
		for i := range nodes {
			if nodes[i].Hostname == hostname {
				found = &nodes[i]
				break
			}
		}
		if found == nil {
			writeErr(w, http.StatusNotFound, "node "+hostname+" not found in swarm")
			return
		}
		if r.Method == http.MethodPost && found.Role != "manager" {
			writeErr(w, http.StatusConflict,
				"refusing to adopt "+hostname+": adoption builds a storage volume, and storage nodes must be managers")
			return
		}

		cur := store.SettingDefault(r.Context(), cluster.SettingStorageAdoptNodes(), "")
		merged := mergeHostnameList(cur, hostname, r.Method == http.MethodPost)
		if err := store.SetSetting(r.Context(), cluster.SettingStorageAdoptNodes(), merged); err != nil {
			writeErr(w, http.StatusInternalServerError, "update storage_adopt_nodes: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"node":     hostname,
			"queued":   r.Method == http.MethodPost,
			"adopting": merged,
			"next": "the node drains itself, copies its data onto the volume, verifies it " +
				"and activates again — watch `pmcluster logs` on that node",
		})
	}
}

// mergeHostnameList adds or removes one hostname from a comma-separated setting.
func mergeHostnameList(cur, hostname string, add bool) string {
	seen := map[string]bool{}
	var kept []string
	for _, p := range strings.Split(cur, ",") {
		p = strings.TrimSpace(p)
		if p == "" || p == hostname || seen[p] {
			continue
		}
		seen[p] = true
		kept = append(kept, p)
	}
	if add {
		kept = append(kept, hostname)
	}
	return strings.Join(kept, ",")
}
