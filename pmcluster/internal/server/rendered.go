package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// RenderedConfigService exposes `GET /api/cluster/rendered` — the cluster's
// platform configs exactly as they were last deployed (templates resolved:
// domain, secrets, certs, host certs). Read-only; the rows are snapshotted
// into the database by every `cluster update` (including the console's
// "Apply to swarm").
type RenderedConfigService struct {
	Store *store.Store
}

func (s *RenderedConfigService) Mount(r chi.Router) {
	r.Get("/cluster/rendered", s.list)
}

func (s *RenderedConfigService) list(res http.ResponseWriter, req *http.Request) {
	if s.Store == nil {
		writeErr(res, http.StatusInternalServerError, "rendered-config service not wired")
		return
	}
	rows, err := s.Store.ListRenderedConfigs(req.Context())
	if err != nil {
		writeErr(res, http.StatusInternalServerError, "list rendered configs: "+err.Error())
		return
	}
	configs := make([]map[string]string, 0, len(rows))
	for _, row := range rows {
		configs = append(configs, map[string]string{"name": row.Name, "content": row.RenderedContent})
	}
	writeJSON(res, http.StatusOK, map[string]any{"configs": configs})
}
