package controllers

import (
	"fmt"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/middleware"
)

// Settings shows and edits the pmcluster API connection plus the cluster-scope
// configs and secrets (the platform's own templates/secrets that `cluster
// update` applies). Editing a cluster config here re-stamps it with the
// current binary version, so the next "Apply to swarm" (or any cluster up /
// update) re-renders the platform stacks from it.
type Settings struct{ *Controller }

type settingsData struct {
	APIURL         string
	EnvAPIURL      string
	HasToken       bool
	HasEnvToken    bool
	Configured     bool
	Version        string
	User           string
	ClusterConfigs []configRow
	Editing        *configRow
	Versions       []configVersionRow
	ClusterSecrets []secretRow
	RevealName     string
	RevealVal      string
	ApplySummary   string
	Error          string
	Msg            string
}

// Page renders the settings fragment with current effective values plus the
// cluster-scope configs and secrets.
func (c Settings) Page(g *gin.Context) {
	apiURL, _, configured := c.loadParams(g.Request.Context())
	d := settingsData{
		APIURL:      apiURL,
		EnvAPIURL:   c.EnvAPI,
		HasEnvToken: c.EnvToken != "",
		Configured:  configured,
		Version:     c.Version,
		User:        username(g),
	}
	if _, err := c.Store.GetSetting(g.Request.Context(), keyToken); err == nil {
		d.HasToken = true
	}
	if configured {
		c.loadCluster(g, &d)
	}
	if name := g.Query("edit"); name != "" && configured {
		if e, vs, err := c.loadConfigEdit(g.Request.Context(), name); err == nil {
			d.Editing, d.Versions = e, vs
		}
	}
	c.Views.Fragment(g, "settings", d)
}

// Save persists API URL/token overrides and applies them immediately.
func (c Settings) Save(g *gin.Context) {
	ctx := g.Request.Context()
	apiURL := g.PostForm("api_url")
	token := g.PostForm("api_token")
	clearToken := g.PostForm("clear_token") == "1"

	if apiURL == "" {
		_ = c.Store.SetSetting(ctx, keyAPIURL, "")
		c.API.SetBase(c.EnvAPI)
	} else {
		_ = c.Store.SetSetting(ctx, keyAPIURL, apiURL)
		c.API.SetBase(apiURL)
	}
	switch {
	case clearToken:
		_ = c.Store.SetSetting(ctx, keyToken, "")
		c.API.SetToken(c.EnvToken)
	case token != "" && token != "•••set•••":
		_ = c.Store.SetSetting(ctx, keyToken, token)
		c.API.SetToken(token)
	}
	c.redirectToSettings(g, "Settings saved.")
}

// AddConfig creates a new cluster-scope config.
func (c Settings) AddConfig(g *gin.Context) {
	ctx := g.Request.Context()
	d := settingsData{}
	name, kind, content := g.PostForm("name"), g.PostForm("kind"), g.PostForm("content")
	if kind == "" {
		kind = "template"
	}
	switch {
	case !c.requireAPI(g, &d.Error):
	case name == "":
		d.Error = "Name is required."
	default:
		if _, err := c.API.CreateConfig(ctx, "cluster", "", name, kind, content); err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = fmt.Sprintf("Config %s created. It is applied by the next cluster update.", name)
		}
	}
	c.reloadSettings(g, d)
}

// EditConfig replaces a cluster config's content, recording a new version and
// re-stamping it with the current binary version so the next cluster update
// re-renders from it.
func (c Settings) EditConfig(g *gin.Context) {
	ctx := g.Request.Context()
	d := settingsData{}
	name, content := g.PostForm("name"), g.PostForm("content")
	switch {
	case !c.requireAPI(g, &d.Error):
	case name == "":
		d.Error = "Invalid config name."
	default:
		if _, err := c.API.UpdateConfig(ctx, name, content); err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = fmt.Sprintf("Config %s updated. Use \"Apply to swarm\" to re-render the platform.", name)
			if e, vs, err := c.loadConfigEdit(ctx, name); err == nil {
				d.Editing, d.Versions = e, vs
			}
		}
	}
	c.reloadSettings(g, d)
}

// RollbackConfig restores a cluster config to a prior version.
func (c Settings) RollbackConfig(g *gin.Context) {
	ctx := g.Request.Context()
	d := settingsData{}
	name := g.Param("name")
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
	c.reloadSettings(g, d)
}

// RemoveConfig deletes a cluster config and its version history.
func (c Settings) RemoveConfig(g *gin.Context) {
	ctx := g.Request.Context()
	d := settingsData{}
	name := g.Param("name")
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
	c.reloadSettings(g, d)
}

// AddSecret stores a new cluster-scope secret.
func (c Settings) AddSecret(g *gin.Context) {
	ctx := g.Request.Context()
	d := settingsData{}
	name, value := g.PostForm("name"), g.PostForm("value")
	switch {
	case !c.requireAPI(g, &d.Error):
	case name == "":
		d.Error = "Name is required."
	case value == "":
		d.Error = "Value is required."
	default:
		if _, err := c.API.CreateSecret(ctx, "cluster", "", name, value); err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = fmt.Sprintf("Secret %s created. The value is stored encrypted; only its hash is shown.", name)
		}
	}
	c.reloadSettings(g, d)
}

// EditSecret replaces a cluster secret's value.
func (c Settings) EditSecret(g *gin.Context) {
	ctx := g.Request.Context()
	d := settingsData{}
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
	c.reloadSettings(g, d)
}

// RemoveSecret deletes a cluster secret by name.
func (c Settings) RemoveSecret(g *gin.Context) {
	ctx := g.Request.Context()
	d := settingsData{}
	name := g.Param("name")
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
	c.reloadSettings(g, d)
}

// RevealSecret decrypts and shows a cluster secret's plaintext on explicit
// request (the UI asks for confirmation first).
func (c Settings) RevealSecret(g *gin.Context) {
	ctx := g.Request.Context()
	d := settingsData{}
	name := g.Param("name")
	switch {
	case !c.requireAPI(g, &d.Error):
	case name == "":
		d.Error = "Invalid secret name."
	default:
		if sv, err := c.API.RevealSecret(ctx, name); err != nil {
			d.Error = err.Error()
		} else {
			d.RevealName, d.RevealVal = sv.Name, sv.Value
		}
	}
	c.reloadSettings(g, d)
}

// Apply triggers a full cluster update on the daemon (content-aware re-apply
// of the platform stacks), surfacing what changed on the swarm side.
func (c Settings) Apply(g *gin.Context) {
	ctx := g.Request.Context()
	d := settingsData{}
	switch {
	case !c.requireAPI(g, &d.Error):
	default:
		sum, err := c.API.TriggerUpdate(ctx)
		if err != nil {
			d.Error = err.Error()
		} else {
			d.ApplySummary = fmt.Sprintf(
				"Cluster update applied — otel: %s, traefik: %s, cert: %s, edge: %s, stacks: %v",
				sum.OTelConfig, sum.TraefikConfig, sum.CertSecret, sum.EdgeConfig, sum.StacksDeployed)
		}
	}
	c.reloadSettings(g, d)
}

// loadCluster loads the cluster-scope configs and secrets into d.
func (c Settings) loadCluster(g *gin.Context, d *settingsData) {
	ctx := g.Request.Context()
	if cfgs, err := c.listConfigsFor(ctx, "cluster", ""); err == nil {
		d.ClusterConfigs = cfgs
	} else if d.Error == "" {
		d.Error = err.Error()
	}
	if secs, err := c.listSecretsFor(ctx, "cluster", ""); err == nil {
		d.ClusterSecrets = secs
	} else if d.Error == "" {
		d.Error = err.Error()
	}
}

// reloadSettings re-renders the settings fragment after an action.
func (c Settings) reloadSettings(g *gin.Context, d settingsData) {
	apiURL, _, configured := c.loadParams(g.Request.Context())
	d.APIURL = apiURL
	d.EnvAPIURL = c.EnvAPI
	d.HasEnvToken = c.EnvToken != ""
	d.Configured = configured
	d.Version = c.Version
	d.User = username(g)
	if _, err := c.Store.GetSetting(g.Request.Context(), keyToken); err == nil {
		d.HasToken = true
	}
	if configured {
		c.loadCluster(g, &d)
	}
	c.Views.Fragment(g, "settings", d)
}

// redirectToSettings reloads the settings fragment with a confirmation message.
func (c Settings) redirectToSettings(g *gin.Context, msg string) {
	d := settingsData{Msg: msg}
	c.reloadSettings(g, d)
}

func username(g *gin.Context) string {
	if u := middleware.CurrentUser(g); u != nil {
		return u.Username
	}
	return ""
}
