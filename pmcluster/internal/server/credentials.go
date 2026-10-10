package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// CredentialsService is the console's view of the bootstrap credentials that
// `cluster up` mints for the bundled components (Traefik dashboard,
// OpenObserve admin, SSO cookie secret).
//
// Deliberately absent: Reveal. The console can list what exists and rotate it;
// it never decrypts a stored password, so password material cannot reach a
// page render, a screenshot, or a trace.
type CredentialsService interface {
	List(ctx context.Context) ([]*store.ManagedCredential, error)
	Rotate(ctx context.Context, name string) (*cluster.ManagedCredential, error)
}

// CredentialsHTTP mounts /api/credentials.
type CredentialsHTTP struct {
	Svc CredentialsService
}

// Mount registers the credentials routes.
func (h *CredentialsHTTP) Mount(r chi.Router) {
	r.Get("/credentials", h.list)
	r.Post("/credentials/{name}/rotate", h.rotate)
}

type credentialRow struct {
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	Username        string `json:"username"`
	SwarmSecretName string `json:"swarm_secret_name,omitempty"`
	CreatedAt       int64  `json:"created_at"`
	RotatedAt       int64  `json:"rotated_at,omitempty"`
}

func (h *CredentialsHTTP) list(res http.ResponseWriter, req *http.Request) {
	creds, err := h.Svc.List(req.Context())
	if err != nil {
		writeErr(res, http.StatusInternalServerError, "list credentials: "+err.Error())
		return
	}
	rows := make([]credentialRow, 0, len(creds))
	for _, c := range creds {
		row := credentialRow{
			Name:            c.Name,
			Kind:            c.Kind,
			Username:        c.Username,
			SwarmSecretName: c.SwarmSecretName,
			CreatedAt:       c.CreatedAt,
		}
		if c.RotatedAt.Valid {
			row.RotatedAt = c.RotatedAt.Int64
		}
		rows = append(rows, row)
	}
	writeJSON(res, http.StatusOK, map[string]any{"credentials": rows})
}

// rotate swaps the credential's password, re-encrypts it, updates the Swarm
// secret and restarts the consuming service. The new password is returned
// exactly once — the operator cannot read it back afterwards (that is what
// keeps Reveal out of this surface).
func (h *CredentialsHTTP) rotate(res http.ResponseWriter, req *http.Request) {
	name := chi.URLParam(req, "name")
	rotated, err := h.Svc.Rotate(req.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrCredentialNotFound) {
			writeErr(res, http.StatusNotFound, "credential not found")
			return
		}
		writeErr(res, http.StatusInternalServerError, "rotate credential: "+err.Error())
		return
	}
	writeJSON(res, http.StatusOK, map[string]any{
		"name":              rotated.Name,
		"username":          rotated.Username,
		"password":          rotated.Password,
		"swarm_secret_name": rotated.SwarmSecretName,
		"username_changed":  rotated.UsernameChanged,
		"shown_once":        true,
	})
}
