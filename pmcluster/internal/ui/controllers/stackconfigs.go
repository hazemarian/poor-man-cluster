package controllers

import (
	"context"
	"fmt"
	"strconv"

	"github.com/gin-gonic/gin"
)

// StackConfigs is the per-stack config + secrets page, reachable from the
// stacks list and stack detail at /stacks/{name}/config. It manages only the
// service-scope configs and secrets that belong to that stack — referenced
// from the DSL via env: VAR: config(<name>) / secrets(<name>).
type StackConfigs struct{ *Controller }

type stackConfigsData struct {
	Stack      string
	Configs    []configRow
	Editing    *configRow
	Versions   []configVersionRow
	Secrets    []secretRow
	RevealName string
	RevealVal  string
	Msg        string
	Error      string
}

// Page renders the config + secrets page for one stack.
func (c StackConfigs) Page(g *gin.Context) {
	ctx := g.Request.Context()
	d := stackConfigsData{Stack: g.Param("name")}
	if !c.requireAPI(g, &d.Error) {
		c.Views.Fragment(g, "stackconfigs", d)
		return
	}
	c.fetch(ctx, &d)
	if name := g.Query("edit"); name != "" {
		if e, vs, err := c.loadConfigEdit(ctx, name); err == nil {
			d.Editing, d.Versions = e, vs
		}
	}
	c.Views.Fragment(g, "stackconfigs", d)
}

// AddConfig creates a new service-scope config for this stack.
func (c StackConfigs) AddConfig(g *gin.Context) {
	ctx := g.Request.Context()
	d := stackConfigsData{Stack: g.Param("name")}
	name, kind, content := g.PostForm("name"), g.PostForm("kind"), g.PostForm("content")
	if kind == "" {
		kind = "file"
	}
	switch {
	case !c.requireAPI(g, &d.Error):
	case name == "":
		d.Error = "Name is required."
	default:
		_, err := c.API.CreateConfig(ctx, "service", d.Stack, name, kind, content)
		if err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = fmt.Sprintf("Config %s created for stack %s.", name, d.Stack)
		}
	}
	c.fetch(ctx, &d)
	c.Views.Fragment(g, "stackconfigs", d)
}

// EditConfig replaces a config's content, recording a new version.
func (c StackConfigs) EditConfig(g *gin.Context) {
	ctx := g.Request.Context()
	d := stackConfigsData{Stack: g.Param("name")}
	name, content := g.PostForm("name"), g.PostForm("content")
	switch {
	case !c.requireAPI(g, &d.Error):
	case name == "":
		d.Error = "Invalid config name."
	default:
		_, err := c.API.UpdateConfig(ctx, name, content)
		if err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = fmt.Sprintf("Config %s updated — a new version was recorded.", name)
			if e, vs, err := c.loadConfigEdit(ctx, name); err == nil {
				d.Editing, d.Versions = e, vs
			}
		}
	}
	c.fetch(ctx, &d)
	c.Views.Fragment(g, "stackconfigs", d)
}

// RollbackConfig restores a config to a prior version.
func (c StackConfigs) RollbackConfig(g *gin.Context) {
	ctx := g.Request.Context()
	d := stackConfigsData{Stack: g.Param("name")}
	name := g.Param("config_name")
	vid, err := strconv.ParseInt(g.Param("version_id"), 10, 64)
	switch {
	case !c.requireAPI(g, &d.Error):
	case err != nil || vid <= 0:
		d.Error = "Invalid version id."
	default:
		if _, err := c.API.RollbackConfig(ctx, name, vid); err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = fmt.Sprintf("Config %s rolled back to version %d.", name, vid)
			if e, vs, err := c.loadConfigEdit(ctx, name); err == nil {
				d.Editing, d.Versions = e, vs
			}
		}
	}
	c.fetch(ctx, &d)
	c.Views.Fragment(g, "stackconfigs", d)
}

// RemoveConfig deletes a config and its version history.
func (c StackConfigs) RemoveConfig(g *gin.Context) {
	ctx := g.Request.Context()
	d := stackConfigsData{Stack: g.Param("name")}
	name := g.Param("config_name")
	switch {
	case !c.requireAPI(g, &d.Error):
	case name == "":
		d.Error = "Invalid config name."
	default:
		if err := c.API.DeleteConfig(ctx, name); err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = fmt.Sprintf("Deleted config %s.", name)
		}
	}
	c.fetch(ctx, &d)
	c.Views.Fragment(g, "stackconfigs", d)
}

// AddSecret stores a new service-scope secret for this stack.
func (c StackConfigs) AddSecret(g *gin.Context) {
	ctx := g.Request.Context()
	d := stackConfigsData{Stack: g.Param("name")}
	name, value := g.PostForm("name"), g.PostForm("value")
	switch {
	case !c.requireAPI(g, &d.Error):
	case name == "":
		d.Error = "Name is required."
	case value == "":
		d.Error = "Value is required."
	default:
		_, err := c.API.CreateSecret(ctx, "service", d.Stack, name, value)
		if err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = fmt.Sprintf("Secret %s created for stack %s.", name, d.Stack)
		}
	}
	c.fetch(ctx, &d)
	c.Views.Fragment(g, "stackconfigs", d)
}

// EditSecret replaces a stored secret's value.
func (c StackConfigs) EditSecret(g *gin.Context) {
	ctx := g.Request.Context()
	d := stackConfigsData{Stack: g.Param("name")}
	name, value := g.PostForm("name"), g.PostForm("value")
	switch {
	case !c.requireAPI(g, &d.Error):
	case name == "":
		d.Error = "Invalid secret name."
	case value == "":
		d.Error = "Value is required."
	default:
		if _, err := c.API.UpdateSecret(ctx, name, value); err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = fmt.Sprintf("Secret %s updated.", name)
		}
	}
	c.fetch(ctx, &d)
	c.Views.Fragment(g, "stackconfigs", d)
}

// RemoveSecret deletes a stored secret by name.
func (c StackConfigs) RemoveSecret(g *gin.Context) {
	ctx := g.Request.Context()
	d := stackConfigsData{Stack: g.Param("name")}
	name := g.Param("secret_name")
	switch {
	case !c.requireAPI(g, &d.Error):
	case name == "":
		d.Error = "Invalid secret name."
	default:
		if err := c.API.DeleteSecret(ctx, name); err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = fmt.Sprintf("Deleted secret %s.", name)
		}
	}
	c.fetch(ctx, &d)
	c.Views.Fragment(g, "stackconfigs", d)
}

// RevealSecret decrypts and shows a stored secret's plaintext on explicit
// request (the UI asks for confirmation first).
func (c StackConfigs) RevealSecret(g *gin.Context) {
	ctx := g.Request.Context()
	d := stackConfigsData{Stack: g.Param("name")}
	name := g.Param("secret_name")
	switch {
	case !c.requireAPI(g, &d.Error):
	case name == "":
		d.Error = "Invalid secret name."
	default:
		sv, err := c.API.RevealSecret(ctx, name)
		if err != nil {
			d.Error = err.Error()
		} else {
			d.RevealName, d.RevealVal = sv.Name, sv.Value
		}
	}
	c.fetch(ctx, &d)
	c.Views.Fragment(g, "stackconfigs", d)
}

// fetch loads the stack's configs + secrets, recording the first error.
func (c StackConfigs) fetch(ctx context.Context, d *stackConfigsData) {
	cfgs, err := c.listConfigsFor(ctx, "service", d.Stack)
	if err != nil && d.Error == "" {
		d.Error = err.Error()
	} else {
		d.Configs = cfgs
	}
	secs, err := c.listSecretsFor(ctx, "service", d.Stack)
	if err != nil && d.Error == "" {
		d.Error = err.Error()
	} else {
		d.Secrets = secs
	}
}

// requireAPI guards a handler until the pmapi connection is configured.
func (c *Controller) requireAPI(g *gin.Context, errOut *string) bool {
	_, _, configured := c.loadParams(g.Request.Context())
	if !configured {
		*errOut = "pmcluster API not configured. Open Settings first."
		return false
	}
	return true
}
