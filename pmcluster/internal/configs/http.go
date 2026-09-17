package configs

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// HTTP exposes the configs service over the Bearer-protected /api router:
// config CRUD + version history, and the read-only rendered snapshots.
type HTTP struct {
	Svc Service
}

func (c *HTTP) Mount(r chi.Router) {
	r.Get("/configs", c.list)
	r.Post("/configs", c.create)
	r.Get("/configs/{name}", c.get)
	r.Put("/configs/{name}", c.update)
	r.Delete("/configs/{name}", c.remove)
	r.Get("/configs/{name}/versions", c.versions)
	r.Post("/configs/{name}/rollback", c.rollback)
	r.Get("/cluster/rendered", c.rendered)
}

type configRow struct {
	ID        int64  `json:"id"`
	Scope     string `json:"scope"`
	Stack     string `json:"stack,omitempty"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Version   string `json:"version"`
	Hash      string `json:"hash"`
	Rendered  bool   `json:"rendered,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

func (c *HTTP) list(res http.ResponseWriter, req *http.Request) {
	cfgs, err := c.Svc.List(req.Context(), req.URL.Query().Get("scope"), req.URL.Query().Get("stack"))
	if err != nil {
		writeErr(res, http.StatusInternalServerError, "list configs: "+err.Error())
		return
	}
	rows := make([]configRow, 0, len(cfgs))
	for _, x := range cfgs {
		rows = append(rows, configRow{
			ID: x.ID, Scope: x.Scope, Stack: x.Stack, Name: x.Name, Kind: x.Kind,
			Version: x.Version, Hash: x.Hash, Rendered: x.Rendered != "",
			CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt,
		})
	}
	writeJSON(res, http.StatusOK, map[string]any{"configs": rows})
}

type createConfigRequest struct {
	Scope   string `json:"scope"`
	Stack   string `json:"stack"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

func (c *HTTP) create(res http.ResponseWriter, req *http.Request) {
	var body createConfigRequest
	dec := json.NewDecoder(http.MaxBytesReader(res, req.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(res, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	scope := strings.TrimSpace(body.Scope)
	kind := strings.TrimSpace(body.Kind)
	stack := strings.TrimSpace(body.Stack)
	if name == "" {
		writeErr(res, http.StatusBadRequest, "name is required")
		return
	}
	if scope == "" {
		scope = "service"
	}
	if scope != "cluster" && scope != "service" {
		writeErr(res, http.StatusBadRequest, "scope must be 'cluster' or 'service'")
		return
	}
	if stack != "" && scope != "service" {
		writeErr(res, http.StatusBadRequest, "stack is only valid for service-scope configs")
		return
	}
	if kind == "" {
		kind = "file"
	}
	if kind != "template" && kind != "file" && kind != "env" {
		writeErr(res, http.StatusBadRequest, "kind must be 'template', 'file' or 'env'")
		return
	}

	id, err := c.Svc.Create(req.Context(), scope, stack, name, kind, body.Content, buildVersion())
	if err != nil {
		if errors.Is(err, store.ErrConfigExists) {
			writeErr(res, http.StatusConflict, "config already exists: "+name)
			return
		}
		writeErr(res, http.StatusInternalServerError, "create config: "+err.Error())
		return
	}
	writeJSON(res, http.StatusCreated, map[string]any{
		"id": id, "scope": scope, "stack": stack, "name": name, "kind": kind,
		"hash": store.ConfigHash(body.Content),
	})
}

func (c *HTTP) get(res http.ResponseWriter, req *http.Request) {
	name := chi.URLParam(req, "name")
	cfg, err := c.Svc.Get(req.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrConfigNotFound) {
			writeErr(res, http.StatusNotFound, "config not found: "+name)
			return
		}
		writeErr(res, http.StatusInternalServerError, "get config: "+err.Error())
		return
	}
	writeJSON(res, http.StatusOK, map[string]any{
		"id": cfg.ID, "scope": cfg.Scope, "name": cfg.Name, "kind": cfg.Kind,
		"version": cfg.Version, "hash": cfg.Hash, "content": cfg.Content,
	})
}

type updateConfigRequest struct {
	Content string `json:"content"`
}

func (c *HTTP) update(res http.ResponseWriter, req *http.Request) {
	name := chi.URLParam(req, "name")
	var body updateConfigRequest
	dec := json.NewDecoder(http.MaxBytesReader(res, req.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(res, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	hash, err := c.Svc.Update(req.Context(), name, body.Content, buildVersion())
	if err != nil {
		if errors.Is(err, store.ErrConfigNotFound) {
			writeErr(res, http.StatusNotFound, "config not found: "+name)
			return
		}
		writeErr(res, http.StatusInternalServerError, "update config: "+err.Error())
		return
	}
	writeJSON(res, http.StatusOK, map[string]any{"name": name, "hash": hash})
}

func (c *HTTP) remove(res http.ResponseWriter, req *http.Request) {
	name := chi.URLParam(req, "name")
	if err := c.Svc.Delete(req.Context(), name); err != nil {
		if errors.Is(err, store.ErrConfigNotFound) {
			writeErr(res, http.StatusNotFound, "config not found: "+name)
			return
		}
		writeErr(res, http.StatusInternalServerError, "delete config: "+err.Error())
		return
	}
	res.WriteHeader(http.StatusNoContent)
}

type configVersionRow struct {
	ID        int64  `json:"id"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"created_at"`
}

func (c *HTTP) versions(res http.ResponseWriter, req *http.Request) {
	name := chi.URLParam(req, "name")
	vers, err := c.Svc.ListVersions(req.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrConfigNotFound) {
			writeErr(res, http.StatusNotFound, "config not found: "+name)
			return
		}
		writeErr(res, http.StatusInternalServerError, "list config versions: "+err.Error())
		return
	}
	rows := make([]configVersionRow, 0, len(vers))
	for _, v := range vers {
		rows = append(rows, configVersionRow(v))
	}
	writeJSON(res, http.StatusOK, map[string]any{"versions": rows})
}

type rollbackConfigRequest struct {
	VersionID int64 `json:"version_id"`
}

func (c *HTTP) rollback(res http.ResponseWriter, req *http.Request) {
	name := chi.URLParam(req, "name")
	var body rollbackConfigRequest
	dec := json.NewDecoder(http.MaxBytesReader(res, req.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(res, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if body.VersionID <= 0 {
		writeErr(res, http.StatusBadRequest, "version_id is required")
		return
	}
	hash, err := c.Svc.Rollback(req.Context(), name, body.VersionID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrConfigNotFound):
			writeErr(res, http.StatusNotFound, "config not found: "+name)
		case errors.Is(err, store.ErrConfigVersionNotFound):
			writeErr(res, http.StatusNotFound, "config version not found")
		default:
			writeErr(res, http.StatusInternalServerError, "rollback config: "+err.Error())
		}
		return
	}
	writeJSON(res, http.StatusOK, map[string]any{"name": name, "hash": hash, "rolled_back_to": body.VersionID})
}

func (c *HTTP) rendered(res http.ResponseWriter, req *http.Request) {
	rows, err := c.Svc.ListRendered(req.Context())
	if err != nil {
		writeErr(res, http.StatusInternalServerError, "list rendered configs: "+err.Error())
		return
	}
	configs := make([]map[string]string, 0, len(rows))
	for _, row := range rows {
		configs = append(configs, map[string]string{"name": row.Name, "content": row.Rendered})
	}
	writeJSON(res, http.StatusOK, map[string]any{"configs": configs})
}

// buildVersion returns the build version string used when writing config
// rows. It is the running binary's version (buildinfo.Version), overridable
// in tests via the setBuildVersion helper.
var buildVersion = func() string { return buildinfo.Version }

func writeJSON(res http.ResponseWriter, status int, v any) {
	res.Header().Set("Content-Type", "application/json")
	res.WriteHeader(status)
	_ = json.NewEncoder(res).Encode(v)
}

func writeErr(res http.ResponseWriter, status int, msg string) {
	writeJSON(res, status, map[string]string{"error": msg})
}
