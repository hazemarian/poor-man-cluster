// Package api implements the pmcluster HTTP handlers.
package api

import (
	"encoding/json"
	"net/http"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
)

// Health is unauthenticated and never touches DB or Docker — liveness
// must not block on dependencies.
func Health(w http.ResponseWriter, _ *http.Request) {
	v, c, _ := buildinfo.Resolve()
	body := map[string]any{
		"status":  "ok",
		"version": v,
		"commit":  c,
	}
	writeJSON(w, http.StatusOK, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
