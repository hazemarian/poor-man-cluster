package usage

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// HTTP exposes the usage domain over REST under /api/usage.
type HTTP struct {
	Svc Service
}

// Mount registers GET /usage.
func (h *HTTP) Mount(r chi.Router) {
	r.Get("/usage", h.get)
}

func (h *HTTP) get(w http.ResponseWriter, r *http.Request) {
	u, err := h.Svc.Get(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "compute usage: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
