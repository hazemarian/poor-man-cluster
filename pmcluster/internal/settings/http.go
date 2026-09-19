package settings

import (
	"encoding/json"
	"net/http"

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
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings})
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
