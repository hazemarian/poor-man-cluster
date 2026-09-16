package controllers

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/pmapi"
)

// configRow is a stored config as shown in the UI (content only when loaded
// for editing). Scope "cluster" configs live in Settings; scope "service"
// configs belong to the stack named in Stack.
type configRow struct {
	ID        int64
	Scope     string
	Stack     string
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
		out = append(out, configRow{ID: c.ID, Scope: c.Scope, Stack: c.Stack, Name: c.Name, Kind: c.Kind, Version: c.Version, Hash: c.Hash, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, Content: c.Content})
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

// listConfigsFor lists stored configs filtered by scope ("" = all) and stack
// ("" = all).
func (c *Controller) listConfigsFor(ctx context.Context, scope, stack string) ([]configRow, error) {
	cfgs, err := c.API.ListConfigs(ctx, scope, stack)
	if err != nil {
		return nil, err
	}
	return configRows(cfgs), nil
}

// loadConfigEdit loads one config's content + version history for the edit
// form.
func (c *Controller) loadConfigEdit(ctx context.Context, name string) (*configRow, []configVersionRow, error) {
	cfg, err := c.API.GetConfig(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	row := configRows([]pmapi.Config{*cfg})[0]
	var vs []configVersionRow
	if v, err := c.API.ConfigVersions(ctx, name); err == nil {
		vs = configVersionRows(v)
	}
	return &row, vs, nil
}

// configFormData drives the shared config create/edit modal fragment
// ("configform"). Action is the POST endpoint the form submits to.
type configFormData struct {
	Scope    string // cluster | service
	Stack    string // owning stack (service scope)
	Name     string
	Kind     string // template | file | env
	Content  string
	IsEdit   bool
	Versions []configVersionRow
	Error    string
	Action   string
}

// configForm renders the config modal fragment. Handlers use it for the
// hx-get "new"/"edit" routes (plain 200 → openModal) and for validation
// failures (422 + HX-Retarget so the error stays inside the modal).
func (c *Controller) configForm(g *gin.Context, d configFormData) {
	if d.Kind == "" {
		d.Kind = "file"
	}
	c.Views.Fragment(g, "configform", d)
}
