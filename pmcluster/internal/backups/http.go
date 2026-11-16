package backups

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// HTTP exposes the on-demand volume backup pipeline over REST.
type HTTP struct {
	Svc Service
	// VolumeRoot resolves the cluster's configured volume_root setting so the
	// restore default follows the operator's layout (nil → "/var/stack/data").
	VolumeRoot func(ctx context.Context) string
	// PlatformExport runs the manual control-plane export (ADR 0007). Nil means the route
	// is not mounted: a daemon without a store or a leader position cannot export.
	PlatformExport func(ctx context.Context) (PlatformExportResult, error)
}

// Mount registers the cluster-scope backup routes on the API router.
func (h *HTTP) Mount(r chi.Router) {
	r.Get("/backups", h.list)
	r.Post("/backups", h.create)
	r.Get("/backups/{id}/files", h.browse)
	r.Post("/backups/{id}/restore", h.restore)
	if h.PlatformExport != nil {
		// The control plane's own restore point, NOT a stack artifact: `backup create` is the
		// volume pipeline, this is the leader's database plus its key (ADR 0007).
		r.Post("/backups/platform", h.platform)
	}
}

// platform exports the leader's control plane offsite now.
//
// The leader refusal is a 409, not a 500: asking the wrong node is a caller-side mistake the
// console can explain ("this node is not the leader"), while a failure to write or upload is a
// 502 the operator should look at.
func (h *HTTP) platform(w http.ResponseWriter, r *http.Request) {
	res, err := h.PlatformExport(r.Context())
	if err != nil {
		if errors.Is(err, ErrNotLeader) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
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
		Volume   string `json:"volume,omitempty"`
		FromS3   bool   `json:"from_s3,omitempty"`
		// Drain is a pointer so "absent" (drain, the default) and an explicit
		// false (--no-drain equivalent) are different requests.
		Drain *bool `json:"drain,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.DestRoot == "" {
		if h.VolumeRoot != nil {
			if vr := h.VolumeRoot(r.Context()); vr != "" {
				body.DestRoot = vr
			}
		}
		if body.DestRoot == "" {
			body.DestRoot = "/var/stack/data"
		}
	}
	// Drain by default: the API is how the console restores, and the console has
	// no flag to tick. `"drain": false` is the escape hatch, mirroring --no-drain.
	drain := true
	if body.Drain != nil {
		drain = *body.Drain
	}
	if drain && h.VolumeRoot != nil {
		if live := h.VolumeRoot(r.Context()); live != "" && !RestoreIntoLiveVolume(body.DestRoot, live) {
			drain = false
		}
	}
	n, err := h.Svc.Restore(r.Context(), id, body.DestRoot, RestoreOptions{Volume: body.Volume, FromS3: body.FromS3, Drain: drain})
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
