package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/deploy"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// StacksHandler routes:
//
//	POST /api/stacks                              — deploy a new revision
//	GET  /api/stacks                              — list
//	GET  /api/stacks/{name}                       — metadata + recent revisions
//	GET  /api/stacks/{name}/revisions/{rev}       — full source + rendered YAML
//	POST /api/stacks/{name}/rollback              — body {revision: N}
type StacksHandler struct {
	Store   *store.Store
	Service *deploy.Service
}

// Mount expects to be wrapped with Bearer auth in the parent router.
func (h *StacksHandler) Mount(r chi.Router) {
	r.Post("/stacks", h.deploy)
	r.Get("/stacks", h.list)
	r.Get("/stacks/{name}", h.show)
	r.Get("/stacks/{name}/revisions/{rev}", h.showRevision)
	r.Post("/stacks/{name}/rollback", h.rollback)
	r.Delete("/stacks/{name}", h.remove)
}

func (h *StacksHandler) deploy(w http.ResponseWriter, r *http.Request) {
	var p deploy.Payload
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	res, err := h.Service.Deploy(r.Context(), p)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stack":    res.StackName,
		"revision": res.Revision,
	})
}

func (h *StacksHandler) list(w http.ResponseWriter, r *http.Request) {
	stacks, err := h.Store.ListStacks(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(stacks))
	for _, s := range stacks {
		out = append(out, stackJSON(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"stacks": out})
}

// show returns metadata + the 20 most recent revisions.
func (h *StacksHandler) remove(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := h.Service.Undeploy(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrStackNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "stack not found"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stack": name})
}

func (h *StacksHandler) show(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	st, err := h.Store.GetStack(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrStackNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "stack not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	revs, err := h.Store.ListRevisions(r.Context(), name, 20)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	revsJSON := make([]map[string]any, 0, len(revs))
	for _, rv := range revs {

		revsJSON = append(revsJSON, map[string]any{
			"revision":   rv.Revision,
			"created_at": rv.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stack":       stackJSON(st),
		"revisions":   revsJSON,
		"last_backup": lastBackupJSON(r.Context(), h.Store, name),
	})
}

// lastBackupJSON returns the most-recent backup row for the stack in a
// shape suitable for JSON serialization, or nil if there are no backups.
// Errors are folded to nil so a hiccup in the audit table doesn't fail
// the stack-show endpoint.
func lastBackupJSON(ctx context.Context, st *store.Store, name string) map[string]any {
	backups, err := st.ListBackupsForStack(ctx, name)
	if err != nil || len(backups) == 0 {
		return nil
	}
	b := backups[0]
	out := map[string]any{
		"status":     b.Status,
		"started_at": b.StartedAt,
	}
	if b.FinishedAt.Valid {
		out["finished_at"] = b.FinishedAt.Int64
	}
	if b.Revision.Valid {
		out["revision"] = b.Revision.Int64
	}
	if b.ErrorMessage != "" {
		out["error_message"] = b.ErrorMessage
	}
	return out
}

// showRevision returns the full source + rendered YAML for one revision.
func (h *StacksHandler) showRevision(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	rev, err := strconv.ParseInt(chi.URLParam(r, "rev"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "revision must be an integer"})
		return
	}
	r2, err := h.Store.GetRevision(r.Context(), name, rev)
	if err != nil {
		if errors.Is(err, store.ErrRevisionNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "revision not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stack":         r2.StackName,
		"revision":      r2.Revision,
		"created_at":    r2.CreatedAt,
		"source_yaml":   r2.SourceYAML,
		"rendered_yaml": r2.RenderedYAML,
		"payload":       r2.PayloadJSON.String,
	})
}

func (h *StacksHandler) rollback(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var body struct {
		Revision int64 `json:"revision"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	if body.Revision == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "revision: required"})
		return
	}
	res, err := h.Service.Rollback(r.Context(), name, body.Revision)
	if err != nil {
		if errors.Is(err, store.ErrRevisionNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "revision not found"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stack":          res.StackName,
		"new_revision":   res.Revision,
		"rolled_back_to": body.Revision,
	})
}

// stackJSON keeps the Stack response shape identical across endpoints.
func stackJSON(s *store.Stack) map[string]any {
	repo := ""
	if s.RepoURL.Valid {
		repo = s.RepoURL.String
	}
	return map[string]any{
		"name":             s.Name,
		"current_revision": s.CurrentRevision,
		"repo_url":         repo,
		"created_at":       s.CreatedAt,
		"updated_at":       s.UpdatedAt,
	}
}
