package stacks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// HTTP serves the stacks REST surface:
//
//	POST /api/stacks                        — deploy a new revision
//	GET  /api/stacks                        — list
//	GET  /api/stacks/{name}                 — metadata + recent revisions
//	GET  /api/stacks/{name}/revisions/{rev} — full source + rendered YAML
//	POST /api/stacks/{name}/rollback        — body {revision: N}
type HTTP struct {
	Deploy  Deployer
	Read    Reader
	Backups backups.Service
}

// Mount expects to be wrapped with Bearer auth in the parent router.
func (h *HTTP) Mount(r chi.Router) {
	r.Post("/stacks", h.deploy)
	r.Get("/stacks", h.list)
	r.Get("/stacks/{name}", h.show)
	r.Get("/stacks/{name}/revisions/{rev}", h.showRevision)
	r.Post("/stacks/{name}/sync", h.sync)
	r.Post("/stacks/{name}/rollback", h.rollback)
	r.Delete("/stacks/{name}", h.remove)
}

func (h *HTTP) deploy(w http.ResponseWriter, r *http.Request) {
	var p Payload
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	res, err := h.Deploy.Deploy(r.Context(), p)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stack":    res.StackName,
		"revision": res.Revision,
	})
}

func (h *HTTP) list(w http.ResponseWriter, r *http.Request) {
	stacks, err := h.Read.List(r.Context())
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

func (h *HTTP) remove(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := h.Deploy.Undeploy(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrStackNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "stack not found"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stack": name})
}

// show returns metadata + the 20 most recent revisions.
func (h *HTTP) show(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	st, err := h.Read.Get(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrStackNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "stack not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	revs, err := h.Read.Revisions(r.Context(), name, 20)
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
		"stack":       stackJSON(*st),
		"revisions":   revsJSON,
		"last_backup": lastBackupJSON(r.Context(), h.Backups, name),
	})
}

// lastBackupJSON returns the most-recent backup run for the stack in a shape
// suitable for JSON serialization, or nil if there are no backups. Errors are
// folded to nil so a hiccup in the audit table doesn't fail the stack-show
// endpoint.
func lastBackupJSON(ctx context.Context, svc backups.Service, name string) map[string]any {
	if svc == nil {
		return nil
	}
	runs, err := svc.ListForStack(ctx, name)
	if err != nil || len(runs) == 0 {
		return nil
	}
	b := runs[0]
	out := map[string]any{
		"status":     b.Status,
		"started_at": b.StartedAt,
	}
	if b.FinishedAt != 0 {
		out["finished_at"] = b.FinishedAt
	}
	if b.Revision != 0 {
		out["revision"] = b.Revision
	}
	if b.ErrorMessage != "" {
		out["error_message"] = b.ErrorMessage
	}
	return out
}

// showRevision returns the full source + rendered YAML for one revision.
func (h *HTTP) showRevision(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	rev, err := strconv.ParseInt(chi.URLParam(r, "rev"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "revision must be an integer"})
		return
	}
	revs, err := h.Read.Revisions(r.Context(), name, 0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	idx := -1
	for i := range revs {
		if revs[i].Revision == rev {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "revision not found"})
		return
	}
	rv := revs[idx]
	writeJSON(w, http.StatusOK, map[string]any{
		"stack":         rv.StackName,
		"revision":      rv.Revision,
		"created_at":    rv.CreatedAt,
		"source_yaml":   rv.SourceYAML,
		"rendered_yaml": rv.RenderedYAML,
		"payload":       rv.PayloadJSON,
	})
}

// sync re-runs the deploy pipeline from the stack's latest stored manifest.
func (h *HTTP) sync(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	res, err := h.Deploy.Sync(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrStackNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "stack not found"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stack":    res.StackName,
		"revision": res.Revision,
		"changed":  res.Changed,
	})
}

func (h *HTTP) rollback(w http.ResponseWriter, r *http.Request) {
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
	res, err := h.Deploy.Rollback(r.Context(), name, body.Revision)
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
func stackJSON(s Stack) map[string]any {
	return map[string]any{
		"name":             s.Name,
		"current_revision": s.CurrentRevision,
		"repo_url":         s.RepoURL,
		"source_file":      s.SourceFile,
		"created_at":       s.CreatedAt,
		"updated_at":       s.UpdatedAt,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
