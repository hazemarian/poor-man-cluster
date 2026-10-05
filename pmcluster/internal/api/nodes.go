package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// NodesHandler returns the list of swarm nodes (one row per `docker node ls`
// entry). Each row carries the node's storage-ness (from the storage_nodes
// cluster setting) so the console can render a promote/demote action.
func NodesHandler(d runtime.Client, store interface {
	GetSettingDefault(ctx context.Context, key, def string) string
	SetSetting(ctx context.Context, key, value string) error
}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		nodes, err := d.NodeList(r.Context())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		storage := map[string]bool{}
		if store != nil {
			for _, h := range strings.Split(store.GetSettingDefault(r.Context(), cluster.SettingStorageNodes(), ""), ",") {
				h = strings.TrimSpace(h)
				if h != "" {
					storage[h] = true
				}
			}
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
				"storage":        storage[n.Hostname],
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"nodes": out})
	}
}

// NodeStorageHandler promotes/demotes a node as a storage node: it updates
// the storage_nodes cluster setting AND stamps/clears the pmcluster.storage
// Swarm node label. POST promotes, DELETE demotes. This is the daemon-side
// twin of `pmcluster node promote/demote` so the console can do it.
func NodeStorageHandler(d runtime.Client, store interface {
	GetSettingDefault(ctx context.Context, key, def string) string
	SetSetting(ctx context.Context, key, value string) error
}) http.HandlerFunc {
	writeErr := func(w http.ResponseWriter, status int, msg string) {
		writeJSON(w, status, map[string]any{"error": msg})
	}
	return func(w http.ResponseWriter, r *http.Request) {
		hostname := chi.URLParam(r, "hostname")
		if hostname == "" {
			writeErr(w, http.StatusBadRequest, "hostname required")
			return
		}
		nodes, err := d.NodeList(r.Context())
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		var node *runtime.Node
		for i := range nodes {
			if nodes[i].Hostname == hostname {
				node = &nodes[i]
				break
			}
		}
		if node == nil {
			writeErr(w, http.StatusNotFound, "node "+hostname+" not found in swarm")
			return
		}

		cur := ""
		if store != nil {
			cur = store.GetSettingDefault(r.Context(), cluster.SettingStorageNodes(), "")
		}

		var merged string
		if r.Method == http.MethodPost {
			merged, _ = appendStorageNodeNames(cur, hostname)
		} else {
			var kept []string
			for _, p := range strings.Split(cur, ",") {
				p = strings.TrimSpace(p)
				if p == "" || p == hostname {
					continue
				}
				kept = append(kept, p)
			}
			merged = strings.Join(kept, ",")
		}

		if store != nil {
			if err := store.SetSetting(r.Context(), cluster.SettingStorageNodes(), merged); err != nil {
				writeErr(w, http.StatusInternalServerError, "update storage_nodes: "+err.Error())
				return
			}
		}
		labelValue := "true"
		if r.Method != http.MethodPost {
			labelValue = ""
		}
		if err := d.SetNodeLabel(r.Context(), node.ID, runtime.StorageNodeLabel, labelValue); err != nil {
			// Setting already written — surface the label failure loudly but
			// keep the response useful (cluster update repairs the label).
			writeErr(w, http.StatusInternalServerError, "update node label: "+err.Error()+" (storage_nodes was updated; run 'pmcluster cluster update' to repair the label)")
			return
		}

		if r.Method == http.MethodPost {
			writeJSON(w, http.StatusOK, map[string]any{
				"hostname":      hostname,
				"storage":       true,
				"storage_nodes": merged,
			})
		} else {
			writeJSON(w, http.StatusOK, map[string]any{
				"hostname":      hostname,
				"storage":       false,
				"storage_nodes": merged,
			})
		}
	}
}

// appendStorageNodeNames appends host to a comma-separated storage_nodes list
// (dedup + drop empties) and reports whether the list changed.
func appendStorageNodeNames(current, host string) (string, bool) {
	seen := map[string]bool{}
	var out []string
	for _, p := range strings.Split(current, ",") {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	changed := !seen[host]
	if changed {
		out = append(out, host)
	}
	return strings.Join(out, ","), changed
}
