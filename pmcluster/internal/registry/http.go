package registry

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// HTTP exposes registry credentials under the Bearer-protected /api router.
// Passwords are write-only: accepted on add, never returned by any route.
type HTTP struct {
	Svc *Service
}

// Mount registers the /api/registries routes.
func (h *HTTP) Mount(r chi.Router) {
	r.Get("/registries", h.list)
	r.Post("/registries", h.add)
	r.Delete("/registries/{host}", h.remove)
}

func (h *HTTP) list(res http.ResponseWriter, req *http.Request) {
	rows, err := h.Svc.List(req.Context())
	if err != nil {
		writeErr(res, http.StatusInternalServerError, "list registries: "+err.Error())
		return
	}
	writeJSON(res, http.StatusOK, map[string]any{"registries": rows})
}

type addRegistryRequest struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *HTTP) add(res http.ResponseWriter, req *http.Request) {
	var body addRegistryRequest
	dec := json.NewDecoder(http.MaxBytesReader(res, req.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(res, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	created, err := h.Svc.Add(req.Context(), body.Host, body.Username, body.Password)
	if err != nil {
		if errors.Is(err, ErrInvalidInput) {
			writeErr(res, http.StatusBadRequest, err.Error())
			return
		}
		// A rejected login or an unreachable registry: the request was
		// well-formed, the upstream said no.
		writeErr(res, http.StatusBadGateway, err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(res, status, map[string]any{
		"host":     strings.TrimSpace(body.Host),
		"username": strings.TrimSpace(body.Username),
		"created":  created,
	})
}

func (h *HTTP) remove(res http.ResponseWriter, req *http.Request) {
	host := chi.URLParam(req, "host")
	warning, err := h.Svc.Remove(req.Context(), host)
	if err != nil {
		if errors.Is(err, store.ErrRegistryNotFound) {
			writeErr(res, http.StatusNotFound, "registry not configured")
			return
		}
		writeErr(res, http.StatusInternalServerError, "remove registry: "+err.Error())
		return
	}
	body := map[string]any{"host": host, "removed": true}
	if warning != "" {
		body["warning"] = warning
	}
	writeJSON(res, http.StatusOK, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
