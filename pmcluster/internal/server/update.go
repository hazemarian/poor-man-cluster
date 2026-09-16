package server

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
)

// UpdateService exposes `POST /api/update` — the operator-triggered swarm
// apply. It runs the same content-aware cluster.Update pipeline that
// `pmcluster cluster up`/`cluster update` uses, so edits made to cluster-scope
// configs in the console reach the swarm on demand.
type UpdateService struct {
	// Update runs the cluster update pipeline and reports what moved.
	Update func(ctx context.Context) (*cluster.UpdateResult, error)
}

func (u *UpdateService) Mount(r chi.Router) {
	r.Post("/update", u.run)
}

func (u *UpdateService) run(res http.ResponseWriter, req *http.Request) {
	if u.Update == nil {
		writeErr(res, http.StatusInternalServerError, "update service not wired")
		return
	}
	resu, err := u.Update(req.Context())
	if err != nil {
		writeErr(res, http.StatusInternalServerError, "cluster update: "+err.Error())
		return
	}
	writeJSON(res, http.StatusOK, map[string]any{
		"otel_config":     resu.OTelConfig,
		"traefik_config":  resu.TraefikConfig,
		"cert_secret":     resu.CertSecret,
		"key_secret":      resu.KeySecret,
		"edge_config":     resu.EdgeConfig,
		"stacks_deployed": resu.StacksDeployed,
	})
}
