package controllers

import (
	"context"
	"fmt"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/pmapi"
)

// Configs lists stored configs (templates/files/env values), lets the operator
// create, edit and delete them, and provides version history + rollback. Config
// content is plaintext (not a secret) and can be referenced from the DSL via
// env: VAR: config(name).
type Configs struct{ *Controller }

type configsData struct {
	Configs  []configRow
	Editing  *configRow // the config currently being edited (from GET ?name=)
	Versions []configVersionRow
	Stack    string // when set (per-stack attach), prefill the create form
	Msg      string
	Error    string
}

type configRow struct {
	ID        int64
	Scope     string
	Name      string
	Kind      string
	Version   string
	Hash      string
	CreatedAt int64
	UpdatedAt int64
	Content   string
}

type configVersionRow struct {
	ID        int64
	Hash      string
	CreatedAt int64
}

func configRows(cfgs []pmapi.Config) []configRow {
	out := make([]configRow, 0, len(cfgs))
	for _, c := range cfgs {
		out = append(out, configRow{ID: c.ID, Scope: c.Scope, Name: c.Name, Kind: c.Kind, Version: c.Version, Hash: c.Hash, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
	}
	return out
}

func configVersionRows(vs []pmapi.ConfigVersion) []configVersionRow {
	out := make([]configVersionRow, 0, len(vs))
	for _, v := range vs {
		out = append(out, configVersionRow{ID: v.ID, Hash: v.Hash, CreatedAt: v.CreatedAt})
	}
	return out
}

// Page renders the configs page. When ?name= is set, it loads that config's
// content (and version history) into the edit form.
func (c Configs) Page(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := configsData{Stack: g.Query("stack")}
	if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
		c.Views.Fragment(g, "configs", d)
		return
	}
	d.Configs = c.fetch(ctx, &d)
	if name := g.Query("name"); name != "" {
		d.Editing = c.loadEdit(ctx, name, &d)
	}
	c.Views.Fragment(g, "configs", d)
}

// Add creates a new config from the create form.
func (c Configs) Add(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := configsData{}

	name := g.PostForm("name")
	scope := g.PostForm("scope")
	kind := g.PostForm("kind")
	content := g.PostForm("content")
	if scope == "" {
		scope = "service"
	}
	if kind == "" {
		kind = "file"
	}

	switch {
	case !configured:
		d.Error = "pmcluster API not configured. Open Settings first."
	case name == "":
		d.Error = "Name is required."
	default:
		_, err := c.API.CreateConfig(ctx, scope, name, kind, content)
		if err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = fmt.Sprintf("Config %s created. Reference it from the DSL with env: VAR: config(%s).", name, name)
		}
	}

	d.Configs = c.fetch(ctx, &d)
	c.Views.Fragment(g, "configs", d)
}

// Edit replaces a config's content. The old content is pushed to version
// history automatically on the daemon.
func (c Configs) Edit(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := configsData{}

	name := g.PostForm("name")
	content := g.PostForm("content")

	switch {
	case !configured:
		d.Error = "pmcluster API not configured. Open Settings first."
	case name == "":
		d.Error = "Invalid config name."
	default:
		_, err := c.API.UpdateConfig(ctx, name, content)
		if err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = fmt.Sprintf("Config %s updated — a new version was recorded.", name)
		}
	}

	d.Configs = c.fetch(ctx, &d)
	if name != "" {
		d.Editing = c.loadEdit(ctx, name, &d)
	}
	c.Views.Fragment(g, "configs", d)
}

// Rollback restores a config to a prior version.
func (c Configs) Rollback(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := configsData{}

	name := g.Param("name")
	vid, err := strconv.ParseInt(g.Param("version_id"), 10, 64)

	switch {
	case !configured:
		d.Error = "pmcluster API not configured. Open Settings first."
	case err != nil || vid <= 0:
		d.Error = "Invalid version id."
	default:
		if _, err := c.API.RollbackConfig(ctx, name, vid); err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = fmt.Sprintf("Config %s rolled back to version %d.", name, vid)
		}
	}

	d.Configs = c.fetch(ctx, &d)
	if name != "" {
		d.Editing = c.loadEdit(ctx, name, &d)
	}
	c.Views.Fragment(g, "configs", d)
}

// Remove deletes a config and its version history.
func (c Configs) Remove(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := configsData{}

	name := g.Param("name")
	switch {
	case !configured:
		d.Error = "pmcluster API not configured. Open Settings first."
	case name == "":
		d.Error = "Invalid config name."
	default:
		if err := c.API.DeleteConfig(ctx, name); err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = fmt.Sprintf("Deleted config %s.", name)
		}
	}

	d.Configs = c.fetch(ctx, &d)
	c.Views.Fragment(g, "configs", d)
}

// fetch lists configs, recording an error if the list itself fails.
func (c Configs) fetch(ctx context.Context, d *configsData) []configRow {
	cfgs, err := c.API.ListConfigs(ctx)
	if err != nil {
		if d.Error == "" {
			d.Error = err.Error()
		}
		return nil
	}
	return configRows(cfgs)
}

// loadEdit loads one config's content + version history for the edit form.
func (c Configs) loadEdit(ctx context.Context, name string, d *configsData) *configRow {
	cfg, err := c.API.GetConfig(ctx, name)
	if err != nil {
		if d.Error == "" {
			d.Error = err.Error()
		}
		return nil
	}
	row := configRows([]pmapi.Config{*cfg})[0]
	if vs, err := c.API.ConfigVersions(ctx, name); err == nil {
		d.Versions = configVersionRows(vs)
	} else if d.Error == "" {
		d.Error = err.Error()
	}
	return &row
}
