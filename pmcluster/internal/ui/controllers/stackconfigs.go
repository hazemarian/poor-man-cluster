package controllers

import (
	"context"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// StackConfigs is the per-stack config + secrets page, reachable from the
// stacks list and the stack inspector at /stacks/{name}/config. It manages
// only the service-scope configs and secrets that belong to that stack —
// referenced from the DSL via env: VAR: config(<name>) / secrets(<name>).
//
// The page keeps three answers apart: values, "unknown" (the API did not
// answer for that source) and "empty" (it answered with nothing). v1 printed
// an empty table for all three.
type StackConfigs struct{ *Controller }

type stackConfigsData struct {
	Stack   string
	Configs []configRow
	Secrets []secretRow

	ConfigsKnown bool
	SecretsKnown bool

	MsgKey  string
	MsgArg0 string
	MsgArg1 string

	ErrKey string
	ErrRaw string
}

// Page renders the config + secrets page for one stack.
func (c StackConfigs) Page(g *gin.Context) {
	d := stackConfigsData{Stack: g.Param("name")}
	c.renderPage(g, d)
}

// ConfigNew renders the create-config form in the modal (service scope).
func (c StackConfigs) ConfigNew(g *gin.Context) {
	d := configFormData{Scope: "service", Stack: g.Param("name"), Kind: "file", Action: WebBase + "/stacks/" + g.Param("name") + "/configs/add"}
	if !c.requireAPI(g, &d.Error) {
		c.configFormError(g, d)
		return
	}
	c.configForm(g, d)
}

// ConfigEdit renders the edit-config form + version history in the modal.
func (c StackConfigs) ConfigEdit(g *gin.Context) {
	ctx := g.Request.Context()
	d := configFormData{Scope: "service", Stack: g.Param("name"), Name: g.Param("config_name"), IsEdit: true, Action: WebBase + "/stacks/" + g.Param("name") + "/configs/edit"}
	if !c.requireAPI(g, &d.Error) {
		c.configFormError(g, d)
		return
	}
	row, vs, err := c.loadConfigEdit(ctx, d.Name)
	if err != nil {
		d.Error = err.Error()
		c.configFormError(g, d)
		return
	}
	d.Kind, d.Content, d.Versions = row.Kind, row.Content, vs
	c.configForm(g, d)
}

// SecretNew renders the create-secret form in the modal (service scope).
func (c StackConfigs) SecretNew(g *gin.Context) {
	d := secretFormData{Scope: "service", Stack: g.Param("name"), Action: WebBase + "/stacks/" + g.Param("name") + "/secrets/add"}
	if !c.requireAPI(g, &d.Error) {
		c.secretFormError(g, d)
		return
	}
	c.secretForm(g, d)
}

// SecretEdit renders the edit-secret form in the modal.
func (c StackConfigs) SecretEdit(g *gin.Context) {
	d := secretFormData{Scope: "service", Stack: g.Param("name"), Name: g.Param("secret_name"), IsEdit: true, Action: WebBase + "/stacks/" + g.Param("name") + "/secrets/edit"}
	if !c.requireAPI(g, &d.Error) {
		c.secretFormError(g, d)
		return
	}
	c.secretForm(g, d)
}

// AddConfig creates a new service-scope config for this stack.
func (c StackConfigs) AddConfig(g *gin.Context) {
	ctx := g.Request.Context()
	stack := g.Param("name")
	d := configFormData{Scope: "service", Stack: stack, Action: WebBase + "/stacks/" + stack + "/configs/add"}
	name, kind, content := g.PostForm("name"), g.PostForm("kind"), g.PostForm("content")
	if kind == "" {
		kind = "file"
	}
	d.Name, d.Kind, d.Content = name, kind, content
	switch {
	case !c.requireAPI(g, &d.Error):
	case name == "":
		d.Error = "Name is required."
	default:
		if _, err := c.API.CreateConfig(ctx, "service", stack, name, kind, content); err != nil {
			d.Error = err.Error()
		} else {
			// Re-read the page for this stack: v1 rendered a page that had
			// never been loaded, so the config it had just created was missing
			// from its own success screen.
			c.renderPage(g, stackConfigsData{
				Stack: stack, MsgKey: "stackconfigs.msg_config_added", MsgArg0: name, MsgArg1: stack,
			})
			return
		}
	}
	c.configFormError(g, d)
}

// EditConfig replaces a config's content, recording a new version.
func (c StackConfigs) EditConfig(g *gin.Context) {
	ctx := g.Request.Context()
	stack := g.Param("name")
	d := configFormData{Scope: "service", Stack: stack, IsEdit: true, Action: WebBase + "/stacks/" + stack + "/configs/edit"}
	name, content := g.PostForm("name"), g.PostForm("content")
	d.Name, d.Content = name, content
	switch {
	case !c.requireAPI(g, &d.Error):
	case name == "":
		d.Error = "Invalid config name."
	default:
		if _, err := c.API.UpdateConfig(ctx, name, content); err != nil {
			d.Error = err.Error()
		} else {
			c.renderPage(g, stackConfigsData{
				Stack: stack, MsgKey: "stackconfigs.msg_config_edited", MsgArg0: name,
			})
			return
		}
	}
	c.configFormError(g, d)
}

// RollbackConfig restores a config to a prior version.
func (c StackConfigs) RollbackConfig(g *gin.Context) {
	ctx := g.Request.Context()
	stack := g.Param("name")
	name := g.Param("config_name")
	vid, err := strconv.ParseInt(g.Param("version_id"), 10, 64)
	d := stackConfigsData{Stack: stack}
	_, _, configured := c.loadParams(ctx)

	switch {
	case !configured:
		d.ErrKey = "err.api_not_configured"
	case name == "":
		d.ErrKey = "stackconfigs.err_bad_name"
	case err != nil || vid <= 0:
		d.ErrKey = "stackconfigs.err_bad_version"
	default:
		if _, err := c.API.RollbackConfig(ctx, name, vid); err != nil {
			d.ErrKey, d.ErrRaw = "err.config_rollback", err.Error()
		} else {
			d.MsgKey, d.MsgArg0 = "stackconfigs.msg_config_rolled", name
			d.MsgArg1 = strconv.FormatInt(vid, 10)
		}
	}
	c.renderPage(g, d)
}

// RemoveConfig deletes a config and its version history.
func (c StackConfigs) RemoveConfig(g *gin.Context) {
	ctx := g.Request.Context()
	stack := g.Param("name")
	name := g.Param("config_name")
	d := stackConfigsData{Stack: stack}
	_, _, configured := c.loadParams(ctx)

	switch {
	case !configured:
		d.ErrKey = "err.api_not_configured"
	case name == "":
		d.ErrKey = "stackconfigs.err_bad_name"
	default:
		if err := c.API.DeleteConfig(ctx, name); err != nil {
			d.ErrKey, d.ErrRaw = "err.config_remove", err.Error()
		} else {
			d.MsgKey, d.MsgArg0 = "stackconfigs.msg_config_removed", name
		}
	}
	c.renderPage(g, d)
}

// AddSecret stores a new service-scope secret for this stack.
func (c StackConfigs) AddSecret(g *gin.Context) {
	ctx := g.Request.Context()
	stack := g.Param("name")
	d := secretFormData{Scope: "service", Stack: stack, Action: WebBase + "/stacks/" + stack + "/secrets/add"}
	name, value := g.PostForm("name"), g.PostForm("value")
	d.Name, d.Value = name, value
	switch {
	case !c.requireAPI(g, &d.Error):
	case name == "":
		d.Error = "Name is required."
	case value == "":
		d.Error = "Value is required."
	default:
		if _, err := c.API.CreateSecret(ctx, "service", stack, name, value); err != nil {
			d.Error = err.Error()
		} else {
			c.renderPage(g, stackConfigsData{
				Stack: stack, MsgKey: "stackconfigs.msg_secret_added", MsgArg0: name, MsgArg1: stack,
			})
			return
		}
	}
	c.secretFormError(g, d)
}

// EditSecret replaces a stored secret's value.
func (c StackConfigs) EditSecret(g *gin.Context) {
	ctx := g.Request.Context()
	stack := g.Param("name")
	d := secretFormData{Scope: "service", Stack: stack, IsEdit: true, Action: WebBase + "/stacks/" + stack + "/secrets/edit"}
	name, value := g.PostForm("name"), g.PostForm("value")
	d.Name, d.Value = name, value
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
			c.renderPage(g, stackConfigsData{
				Stack: stack, MsgKey: "stackconfigs.msg_secret_edited", MsgArg0: name,
			})
			return
		}
	}
	c.secretFormError(g, d)
}

// RemoveSecret deletes a stored secret by name.
func (c StackConfigs) RemoveSecret(g *gin.Context) {
	ctx := g.Request.Context()
	stack := g.Param("name")
	name := g.Param("secret_name")
	d := stackConfigsData{Stack: stack}
	_, _, configured := c.loadParams(ctx)

	switch {
	case !configured:
		d.ErrKey = "err.api_not_configured"
	case name == "":
		d.ErrKey = "stackconfigs.err_bad_name"
	default:
		if err := c.API.DeleteSecret(ctx, name); err != nil {
			d.ErrKey, d.ErrRaw = "err.secret_remove", err.Error()
		} else {
			d.MsgKey, d.MsgArg0 = "stackconfigs.msg_secret_removed", name
		}
	}
	c.renderPage(g, d)
}

// RevealSecret decrypts and shows a stored secret's plaintext in the modal on
// explicit request (the UI asks for confirmation first).
func (c StackConfigs) RevealSecret(g *gin.Context) {
	ctx := g.Request.Context()
	name := g.Param("secret_name")
	if !c.requireAPI(g, new(string)) {
		c.Views.Fragment(g, "secretreveal", secretRevealData{Name: name, Error: "pmcluster API not configured. Open Settings first."})
		return
	}
	if name == "" {
		c.Views.Fragment(g, "secretreveal", secretRevealData{Error: "Invalid secret name."})
		return
	}
	sv, err := c.API.RevealSecret(ctx, name)
	if err != nil {
		c.Views.Fragment(g, "secretreveal", secretRevealData{Name: name, Error: err.Error()})
		return
	}
	c.Views.Fragment(g, "secretreveal", secretRevealData{Name: sv.Name, Value: sv.Value})
}

// renderPage loads a stack's configs + secrets and renders the page with the
// message or error it was handed. An action error wins over a read error: the
// action is what the operator just did.
func (c StackConfigs) renderPage(g *gin.Context, d stackConfigsData) {
	ctx := g.Request.Context()
	if _, _, configured := c.loadParams(ctx); !configured {
		d.ErrKey = "err.api_not_configured"
		c.Views.Fragment(g, "stackconfigs", d)
		return
	}
	c.fetch(ctx, &d)
	c.Views.Fragment(g, "stackconfigs", d)
}

// configFormError renders the config form with the error, keeping the modal
// open (HX-Retarget + 422 so hx-on::after-request does not close it).
func (c StackConfigs) configFormError(g *gin.Context, d configFormData) {
	g.Header("HX-Retarget", "#modal-body")
	g.Status(http.StatusUnprocessableEntity)
	c.configForm(g, d)
}

// secretFormError renders the secret form with the error, keeping the modal
// open (HX-Retarget + 422 so hx-on::after-request does not close it).
func (c StackConfigs) secretFormError(g *gin.Context, d secretFormData) {
	g.Header("HX-Retarget", "#modal-body")
	g.Status(http.StatusUnprocessableEntity)
	c.secretForm(g, d)
}

// fetch loads the stack's configs + secrets. Each source records its own
// unknown state, so a failed call empties neither table silently.
func (c StackConfigs) fetch(ctx context.Context, d *stackConfigsData) {
	cfgs, err := c.listConfigsFor(ctx, "service", d.Stack)
	if err != nil {
		if d.ErrKey == "" {
			d.ErrKey, d.ErrRaw = "err.stackconfigs_configs", err.Error()
		}
	} else {
		d.ConfigsKnown = true
		d.Configs = cfgs
	}

	secs, err := c.listSecretsFor(ctx, "service", d.Stack)
	if err != nil {
		if d.ErrKey == "" {
			d.ErrKey, d.ErrRaw = "err.stackconfigs_secrets", err.Error()
		}
	} else {
		d.SecretsKnown = true
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
