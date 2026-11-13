package settings

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
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
	// Secret-ish keys are NEVER returned, not even to an admin bearer.
	//
	// They used to be masked only for non-admins, on the theory that the console
	// and the CLI hold the admin token and need the cleartext. That reasoning
	// only covers the transport: the console is a BROWSER, so an admin-masked
	// response puts the S3 secret and the SSO client secret into a page, a
	// devtools panel and the browser's memory — for a value that no form ever
	// needs to show. Reading a secret you already stored is not an operation
	// anybody performs; writing a new one is.
	//
	// So the response carries the shape, not the material: each secret key comes
	// back as Mask, and secret_set says whether one is stored, which is the only
	// fact the form needs to render "set / not set".
	secretsSet := map[string]bool{}
	for k, v := range settings {
		if IsSecretKey(k) && v != "" {
			secretsSet[k] = true
		}
	}
	settings = maskSecrets(settings)
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings, "secret_set": secretsSet})
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
	// A form rendered from a masked GET submits the mask back. Treat a masked or
	// empty value on a secret key as "leave it alone": the alternative is a save
	// that silently erases the S3 secret because the page was never told it.
	for k, v := range body.Settings {
		if IsSecretKey(k) && (strings.TrimSpace(v) == "" || v == Mask) {
			delete(body.Settings, k)
		}
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
