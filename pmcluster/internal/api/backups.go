package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// BackupsHandler exposes the on-demand volume backup pipeline over HTTP.
type BackupsHandler struct {
	Svc service.BackupsService
}

func (h *BackupsHandler) Mount(r chi.Router) {
	r.Get("/backups", h.list)
	r.Post("/backups", h.create)
}

// MountStackScoped is split out so the route can attach to the same
// /stacks subtree as StacksHandler.
func (h *BackupsHandler) MountStackScoped(r chi.Router) {
	r.Get("/stacks/{name}/backups", h.listForStack)
}

type backupDTO struct {
	ID           int64    `json:"id"`
	Status       string   `json:"status"`
	StackName    string   `json:"stack_name,omitempty"`
	Revision     int64    `json:"revision,omitempty"`
	ArchivePaths []string `json:"archive_paths,omitempty"`
	ErrorMessage string   `json:"error_message,omitempty"`
	StartedAt    int64    `json:"started_at"`
	FinishedAt   int64    `json:"finished_at,omitempty"`
}

func toDTO(b *store.Backup) backupDTO {
	d := backupDTO{
		ID:           b.ID,
		Status:       b.Status,
		ArchivePaths: splitArchivePaths(b.ArchivePaths),
		ErrorMessage: b.ErrorMessage,
		StartedAt:    b.StartedAt,
	}
	if b.StackName.Valid {
		d.StackName = b.StackName.String
	}
	if b.Revision.Valid {
		d.Revision = b.Revision.Int64
	}
	if b.FinishedAt.Valid {
		d.FinishedAt = b.FinishedAt.Int64
	}
	return d
}

func splitArchivePaths(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func (h *BackupsHandler) list(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	rows, err := h.Svc.List(r.Context(), limit)
	if err != nil {
		writeBackupErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]backupDTO, 0, len(rows))
	for _, b := range rows {
		out = append(out, toDTO(b))
	}
	writeBackupJSON(w, http.StatusOK, map[string]any{"backups": out})
}

func (h *BackupsHandler) listForStack(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	rows, err := h.Svc.ListForStack(r.Context(), name)
	if err != nil {
		writeBackupErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]backupDTO, 0, len(rows))
	for _, b := range rows {
		out = append(out, toDTO(b))
	}
	writeBackupJSON(w, http.StatusOK, map[string]any{"backups": out})
}

func (h *BackupsHandler) create(w http.ResponseWriter, r *http.Request) {
	id, paths, err := h.Svc.Trigger(r.Context(), "", 0)
	if err != nil {
		if errors.Is(err, service.ErrBackupTriggerNotConfigured) {
			writeBackupErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeBackupErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeBackupJSON(w, http.StatusOK, map[string]any{
		"id":            id,
		"status":        "succeeded",
		"archive_paths": paths,
	})
}

func writeBackupJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeBackupErr(w http.ResponseWriter, status int, msg string) {
	writeBackupJSON(w, status, map[string]any{"error": msg})
}
