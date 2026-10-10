package settings

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
)

// HTTP exposes the settings domain over REST under /api/cluster/settings.
type HTTP struct {
	Svc Service
}

// Mount registers GET/PUT /cluster/settings.
func (h *HTTP) Mount(r chi.Router) {
	r.Get("/cluster/settings", h.get)
	r.Put("/cluster/settings", h.put)
}

func (h *HTTP) get(w http.ResponseWriter, r *http.Request) {
	settings, err := h.Svc.Get(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get settings: "+err.Error())
		return
	}
	// Secret-ish keys (sso_client_secret, backup_s3_* key/secret) are masked
	// for non-admin bearers — mirroring the CLI's masking. The CLI and edge
	// console read the full values with the admin token.
	if !auth.IsAdmin(auth.FromContext(r.Context())) {
		settings = maskSecrets(settings)
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings})
}

// maskSecrets replaces the value of any secret-ish setting (per the shared
// IsSecretKey/MaskValue policy in mask.go) with a fixed mask, so a non-admin
// bearer sees the keys but never their values. Empty values stay empty. The
// caller decides whether to apply it (admin callers don't).
func maskSecrets(settings Settings) Settings {
	out := make(Settings, len(settings))
	for k, v := range settings {
		out[k] = MaskValue(k, v, false)
	}
	return out
}

func (h *HTTP) put(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Settings Settings `json:"settings"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	settings, err := h.Svc.Update(r.Context(), body.Settings)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
