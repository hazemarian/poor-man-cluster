package backups

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// HTTP exposes the on-demand volume backup pipeline over REST.
type HTTP struct {
	Svc Service
}

func (h *HTTP) Mount(r chi.Router) {
	r.Get("/backups", h.list)
	r.Post("/backups", h.create)
	r.Get("/backups/{id}/files", h.browse)
	r.Post("/backups/{id}/restore", h.restore)
}

// MountStackScoped is split out so the route can attach to the same
// /stacks subtree as the stacks handler.
func (h *HTTP) MountStackScoped(r chi.Router) {
	r.Get("/stacks/{name}/backups", h.listForStack)
}

func (h *HTTP) list(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	rows, err := h.Svc.List(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": rows})
}

func (h *HTTP) listForStack(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	rows, err := h.Svc.ListForStack(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": rows})
}

func (h *HTTP) create(w http.ResponseWriter, r *http.Request) {
	id, paths, err := h.Svc.Trigger(r.Context(), "", 0)
	if err != nil {
		if errors.Is(err, ErrTriggerNotConfigured) {
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            id,
		"status":        "succeeded",
		"archive_paths": paths,
	})
}

func (h *HTTP) browse(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid backup id")
		return
	}
	run, files, err := h.Svc.Browse(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "files": files})
}

func (h *HTTP) restore(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid backup id")
		return
	}
	var body struct {
		DestRoot string `json:"dest_root"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.DestRoot == "" {
		body.DestRoot = "/var/stack/data"
	}
	n, err := h.Svc.Restore(r.Context(), id, body.DestRoot)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restored": n, "dest_root": body.DestRoot})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
