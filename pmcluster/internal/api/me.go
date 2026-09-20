package api

import (
	"net/http"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
)

// Me returns the authenticated user's basic profile. Always behind the
// Bearer middleware — the auth.User is guaranteed to be present.
func Me(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	if u == nil {

		http.Error(w, "missing authenticated user (server misconfiguration)", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":   u.ID,
		"name": u.Name,
	})
}
