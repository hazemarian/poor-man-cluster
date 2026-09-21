package controllers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/middleware"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
)

// Settings shows and edits the pmcluster API connection plus the cluster-scope
// configs and secrets (the platform's own templates/secrets that `cluster
// update` applies). Editing a cluster config here re-stamps it with the current
// binary version, so the next "Apply to swarm" (or any cluster up / update)
// re-renders the platform stacks from it.
//
// Three states are load-bearing on this page and are modelled separately:
//
//   - Configured: there is a daemon to ask. When it is false the page says so
//     instead of showing an empty config/secret list.
//   - ConfigsKnown / SecretsKnown: the list call answered. A failure leaves the
//     flag false with ErrKey/ErrRaw filled, so a broken daemon never renders as
//     "nothing configured yet".
//   - ApplyDone: this console did trigger a platform update. Without it, an
//     apply that changed nothing would be indistinguishable from no apply.
type Settings struct{ *Controller }

// tokenPlaceholder is what the token field shows when a token is already stored:
// the stored value never travels back to the browser.
const tokenPlaceholder = "•••set•••"

// settingsData is the page model. ErrKey/ErrRaw describe a page-level failure
// (a failed save, a failed apply); the per-list pairs describe a failed list so
// the two disclosures never overwrite each other.
type settingsData struct {
	APIURL      string
	EnvAPIURL   string
	HasToken    bool
	HasEnvToken bool
	Configured  bool
	Version     string
	User        string

	ClusterConfigs []configRow
	ConfigsKnown   bool
	ConfigsErrKey  string
	ConfigsErrRaw  string

	ClusterSecrets []secretRow
	SecretsKnown   bool
	SecretsErrKey  string
	SecretsErrRaw  string

	ApplyDone    bool
	ApplyOTel    string
	ApplyTraefik string
	ApplyCert    string
	ApplyEdge    string
	ApplyStacks  string

	// CanEditCluster gates the cluster-settings link: the route is admin-only,
	// and a card that leads to a 403 is worse than no card.
	CanEditCluster bool

	ErrKey  string
	ErrRaw  string
	MsgKey  string
	MsgArg  string
	MsgArg2 string
}

// failer is the error pair every access page model carries. Handlers fill it
// through apiRefused and their own error paths so no fragment is ever handed a
// failure it cannot describe.
type failer interface {
	fail(key, raw string)
}

func (d *settingsData) fail(key, raw string) { d.ErrKey, d.ErrRaw = key, raw }

// apiRefused fills d's error pair when the console has no daemon to ask, and
// reports whether the handler must stop. Every access route goes through it, so
// "not configured" is always a stated error rather than an empty list.
func (c *Controller) apiRefused(g *gin.Context, d failer) bool {
	var prose string
	if c.requireAPI(g, &prose) {
		return false
	}
	d.fail(errKeyNotConfigured, prose)
	return true
}

// firstErr keeps the first failure of a sequence whose steps all have to run:
// Save applies the URL and the token even when storing one of them failed.
func firstErr(cur, next error) error {
	if cur != nil {
		return cur
	}
	return next
}

// Page renders the settings fragment with current effective values plus the
// cluster-scope configs and secrets.
func (c Settings) Page(g *gin.Context) {
	d := c.fill(g, settingsData{})
	c.Views.Fragment(g, "settings", d)
}

// Save persists API URL/token overrides and applies them immediately.
func (c Settings) Save(g *gin.Context) {
	ctx := g.Request.Context()
	apiURL := g.PostForm("api_url")
	token := g.PostForm("api_token")
	clearToken := g.PostForm("clear_token") == "1"

	d := settingsData{}
	var err error
	if apiURL == "" {
		err = firstErr(err, c.Store.SetSetting(ctx, keyAPIURL, ""))
		c.API.SetBase(c.EnvAPI)
	} else {
		err = firstErr(err, c.Store.SetSetting(ctx, keyAPIURL, apiURL))
		c.API.SetBase(apiURL)
	}
	switch {
	case clearToken:
		err = firstErr(err, c.Store.SetSetting(ctx, keyToken, ""))
		c.API.SetToken(c.EnvToken)
	case token != "" && token != tokenPlaceholder:
		err = firstErr(err, c.Store.SetSetting(ctx, keyToken, token))
		c.API.SetToken(token)
	}
	if err != nil {
		d.fail("settings.err_save", err.Error())
	} else {
		d.MsgKey = "settings.msg_saved"
	}
	c.Views.Fragment(g, "settings", c.fill(g, d))
}

// ConfigNew renders the create-config form into the modal.
func (c Settings) ConfigNew(g *gin.Context) {
	d := configFormData{Scope: "cluster", Kind: "template", Action: WebBase + "/settings/configs/add"}
	if c.apiRefused(g, &d) {
		c.configFormError(g, d)
		return
	}
	c.configForm(g, d)
}

// ConfigEdit renders the edit-config form (with version history) into the modal.
func (c Settings) ConfigEdit(g *gin.Context) {
	name := g.Param("name")
	d := configFormData{Scope: "cluster", Name: name, Kind: "template", IsEdit: true, Action: WebBase + "/settings/configs/edit"}
	if c.apiRefused(g, &d) {
		c.configFormError(g, d)
		return
	}
	row, versions, err := c.loadConfigEdit(g.Request.Context(), name)
	if err != nil {
		d.fail("configs.err_action", err.Error())
		c.configFormError(g, d)
		return
	}
	d.Name, d.Kind, d.Content, d.Versions = row.Name, row.Kind, row.Content, versions
	c.configForm(g, d)
}

// AddConfig creates a new cluster-scope config. Validation/API failures are
// returned as the form fragment (HX-Retarget #modal-body, 422) so the modal
// stays open with the operator's input intact.
func (c Settings) AddConfig(g *gin.Context) {
	ctx := g.Request.Context()
	name, kind, content := g.PostForm("name"), g.PostForm("kind"), g.PostForm("content")
	if kind == "" {
		kind = "template"
	}
	d := configFormData{Scope: "cluster", Name: name, Kind: kind, Content: content, Action: WebBase + "/settings/configs/add"}
	switch {
	case c.apiRefused(g, &d):
	case name == "":
		d.fail("configs.err_name_required", "")
	default:
		if _, err := c.API.CreateConfig(ctx, "cluster", "", name, kind, content); err != nil {
			d.fail("settings.err_config_action", err.Error())
		} else {
			c.renderWith(g, settingsData{MsgKey: "settings.msg_config_created", MsgArg: name})
			return
		}
	}
	c.configFormError(g, d)
}

// EditConfig replaces a cluster config's content, recording a new version and
// re-stamping it with the current binary version so the next cluster update
// re-renders from it.
func (c Settings) EditConfig(g *gin.Context) {
	ctx := g.Request.Context()
	name, content := g.PostForm("name"), g.PostForm("content")
	d := configFormData{Scope: "cluster", Name: name, Kind: "template", Content: content, IsEdit: true, Action: WebBase + "/settings/configs/edit"}
	switch {
	case c.apiRefused(g, &d):
	case name == "":
		d.fail("settings.err_bad_config_name", "")
	default:
		if _, err := c.API.UpdateConfig(ctx, name, content); err != nil {
			d.fail("settings.err_config_action", err.Error())
		} else {
			c.renderWith(g, settingsData{MsgKey: "settings.msg_config_updated", MsgArg: name})
			return
		}
	}
	c.configFormError(g, d)
}

// RollbackConfig restores a cluster config to a prior version.
func (c Settings) RollbackConfig(g *gin.Context) {
	ctx := g.Request.Context()
	d := settingsData{}
	name := g.Param("name")
	vid, err := strconv.ParseInt(g.Param("version_id"), 10, 64)
	switch {
	case c.apiRefused(g, &d):
	case err != nil || vid <= 0:
		d.fail("settings.err_bad_version", "")
	default:
		if _, err := c.API.RollbackConfig(ctx, name, vid); err != nil {
			d.fail("settings.err_config_action", err.Error())
		} else {
			d.MsgKey = "settings.msg_config_rolled_back"
			d.MsgArg, d.MsgArg2 = name, strconv.FormatInt(vid, 10)
		}
	}
	c.Views.Fragment(g, "settings", c.fill(g, d))
}

// RemoveConfig deletes a cluster config and its version history.
func (c Settings) RemoveConfig(g *gin.Context) {
	ctx := g.Request.Context()
	d := settingsData{}
	name := g.Param("name")
	switch {
	case c.apiRefused(g, &d):
	case name == "":
		d.fail("settings.err_bad_config_name", "")
	default:
		if err := c.API.DeleteConfig(ctx, name); err != nil {
			d.fail("settings.err_config_action", err.Error())
		} else {
			d.MsgKey, d.MsgArg = "settings.msg_config_removed", name
		}
	}
	c.Views.Fragment(g, "settings", c.fill(g, d))
}

// SecretNew renders the create-secret form into the modal.
func (c Settings) SecretNew(g *gin.Context) {
	d := secretFormData{Scope: "cluster", Action: WebBase + "/settings/secrets/add"}
	if c.apiRefused(g, &d) {
		c.secretFormError(g, d)
		return
	}
	c.secretForm(g, d)
}

// SecretEdit renders the edit-secret form into the modal (the stored value is
// never shown — editing replaces it).
func (c Settings) SecretEdit(g *gin.Context) {
	d := secretFormData{Scope: "cluster", Name: g.Param("name"), IsEdit: true, Action: WebBase + "/settings/secrets/edit"}
	if c.apiRefused(g, &d) {
		c.secretFormError(g, d)
		return
	}
	c.secretForm(g, d)
}

// AddSecret stores a new cluster-scope secret. Failures keep the modal open
// with the form fragment.
func (c Settings) AddSecret(g *gin.Context) {
	ctx := g.Request.Context()
	name, value := g.PostForm("name"), g.PostForm("value")
	d := secretFormData{Scope: "cluster", Name: name, Value: value, Action: WebBase + "/settings/secrets/add"}
	switch {
	case c.apiRefused(g, &d):
	case name == "":
		d.fail("secrets.err_name_required", "")
	case value == "":
		d.fail("secrets.err_value_required", "")
	default:
		if _, err := c.API.CreateSecret(ctx, "cluster", "", name, value); err != nil {
			d.fail("settings.err_secret_action", err.Error())
		} else {
			c.renderWith(g, settingsData{MsgKey: "settings.msg_secret_created", MsgArg: name})
			return
		}
	}
	c.secretFormError(g, d)
}

// EditSecret replaces a cluster secret's value.
func (c Settings) EditSecret(g *gin.Context) {
	ctx := g.Request.Context()
	name, value := g.PostForm("name"), g.PostForm("value")
	d := secretFormData{Scope: "cluster", Name: name, Value: value, IsEdit: true, Action: WebBase + "/settings/secrets/edit"}
	switch {
	case c.apiRefused(g, &d):
	case name == "":
		d.fail("settings.err_bad_secret_name", "")
	case value == "":
		d.fail("secrets.err_value_required", "")
	default:
		if _, err := c.API.UpdateSecret(ctx, name, value); err != nil {
			d.fail("settings.err_secret_action", err.Error())
		} else {
			c.renderWith(g, settingsData{MsgKey: "settings.msg_secret_updated", MsgArg: name})
			return
		}
	}
	c.secretFormError(g, d)
}

// RemoveSecret deletes a cluster secret by name.
func (c Settings) RemoveSecret(g *gin.Context) {
	ctx := g.Request.Context()
	d := settingsData{}
	name := g.Param("name")
	switch {
	case c.apiRefused(g, &d):
	case name == "":
		d.fail("settings.err_bad_secret_name", "")
	default:
		if err := c.API.DeleteSecret(ctx, name); err != nil {
			d.fail("settings.err_secret_action", err.Error())
		} else {
			d.MsgKey, d.MsgArg = "settings.msg_secret_removed", name
		}
	}
	c.Views.Fragment(g, "settings", c.fill(g, d))
}

// RevealSecret decrypts and shows a cluster secret's plaintext in the modal.
// The fragment is only ever requested by a button that asked for confirmation
// first, and the value exists nowhere else in the console — it is not part of
// any list, and closing the modal drops it.
func (c Settings) RevealSecret(g *gin.Context) {
	ctx := g.Request.Context()
	name := g.Param("name")
	d := secretRevealData{Name: name}
	switch {
	case c.apiRefused(g, &d):
	case name == "":
		d.fail("settings.err_bad_secret_name", "")
	default:
		sv, err := c.API.RevealSecret(ctx, name)
		if err != nil {
			d.fail("secrets.err_reveal", err.Error())
		} else {
			d.Name, d.Value = sv.Name, sv.Value
		}
	}
	c.Views.Fragment(g, "secretreveal", d)
}

// Apply triggers a full cluster update on the daemon (content-aware re-apply of
// the platform stacks), surfacing what changed on the swarm side.
func (c Settings) Apply(g *gin.Context) {
	ctx := g.Request.Context()
	d := settingsData{}
	if !c.apiRefused(g, &d) {
		sum, err := c.API.TriggerUpdate(ctx)
		if err != nil {
			d.fail("settings.err_apply", err.Error())
		} else {
			d.ApplyDone = true
			d.ApplyOTel, d.ApplyTraefik = sum.OTelConfig, sum.TraefikConfig
			d.ApplyCert, d.ApplyEdge = sum.CertSecret, sum.EdgeConfig
			// StacksDeployed is a list; the summary line reads as prose, so an
			// empty list has to say "none" rather than print "[]".
			if len(sum.StacksDeployed) == 0 {
				d.ApplyStacks = "none"
			} else {
				d.ApplyStacks = strings.Join(sum.StacksDeployed, ", ")
			}
		}
	}
	c.Views.Fragment(g, "settings", c.fill(g, d))
}

// clusterSettingKeys are the editable cluster settings, in form order. They
// must mirror the daemon's /api/cluster/settings allowlist exactly.
var clusterSettingKeys = []string{
	"volume_root",
	"backup_all_nodes",
	"sso_enabled",
	"sso_provider",
	"sso_client_id",
	"sso_client_secret",
	"sso_github_org",
	"sso_cookie_expire",
	"edge_login_disabled",
	"domain",
	"oo_admin_email",
	"traefik_admin_user",
}

// clusterBoolSettings are the allowlisted settings that are flags rather than
// strings, so the form and the save agree on what a cleared checkbox means.
var clusterBoolSettings = map[string]bool{
	"backup_all_nodes":    true,
	"sso_enabled":         true,
	"edge_login_disabled": true,
}

type clusterSettingsData struct {
	Settings        map[string]string
	HasClientSecret bool
	ErrKey          string
	ErrRaw          string
	MsgKey          string
}

// maskClusterSettings hides sso_client_secret from the rendered form: the
// daemon returns it (so edits round-trip), but the console only ever shows it
// masked.
func maskClusterSettings(settings map[string]string) (map[string]string, bool) {
	if settings == nil {
		return nil, false
	}
	out := make(map[string]string, len(settings))
	for k, v := range settings {
		if k == "sso_client_secret" {
			continue
		}
		out[k] = v
	}
	return out, settings["sso_client_secret"] != ""
}

// ClusterSettingsPage renders the editable cluster settings form (admin-only).
func (c Settings) ClusterSettingsPage(g *gin.Context) {
	d := clusterSettingsData{}
	if _, _, configured := c.loadParams(g.Request.Context()); !configured {
		d.ErrKey = "err.api_not_configured"
		c.Views.Fragment(g, "clustersettings", d)
		return
	}
	settings, err := c.API.GetClusterSettings(g.Request.Context())
	if err != nil {
		d.ErrKey, d.ErrRaw = "settings.err_cluster_read", err.Error()
		c.Views.Fragment(g, "clustersettings", d)
		return
	}
	d.Settings, d.HasClientSecret = maskClusterSettings(settings)
	c.Views.Fragment(g, "clustersettings", d)
}

// ClusterSettingsSave persists the edited cluster settings (admin-only). The
// changes are stored but NOT applied — `cluster update` (Apply to swarm)
// applies them.
func (c Settings) ClusterSettingsSave(g *gin.Context) {
	ctx := g.Request.Context()
	d := clusterSettingsData{}
	if _, _, configured := c.loadParams(ctx); !configured {
		d.ErrKey = "err.api_not_configured"
		c.Views.Fragment(g, "clustersettings", d)
		return
	}

	settings := make(map[string]string, len(clusterSettingKeys))
	for _, k := range clusterSettingKeys {
		v := g.PostForm(k)
		// A checkbox that was cleared sends nothing, and storing "" would leave
		// the daemon to decide what an empty flag means. Bools are written as
		// the words the daemon reads: "true" or "false".
		if clusterBoolSettings[k] {
			v = "false"
			if g.PostForm(k) != "" {
				v = "true"
			}
		}
		settings[k] = v
	}
	// The password field is left empty when the operator didn't change the
	// secret — omit it so the stored secret is preserved.
	if settings["sso_client_secret"] == "" {
		delete(settings, "sso_client_secret")
	}

	updated, err := c.API.UpdateClusterSettings(ctx, settings)
	if err != nil {
		d.ErrKey, d.ErrRaw = "settings.err_cluster_save", err.Error()
		d.Settings, d.HasClientSecret = maskClusterSettings(settings)
	} else {
		d.Settings, d.HasClientSecret = maskClusterSettings(updated)
		d.MsgKey = "settings.msg_cluster_saved"
	}
	c.Views.Fragment(g, "clustersettings", d)
}

// configFormError keeps the modal open with the form fragment on a
// validation/API failure (HX-Retarget sends the fragment to #modal-body; the
// 422 status makes hx-on::after-request see event.detail.failed).
func (c Settings) configFormError(g *gin.Context, d configFormData) {
	g.Header("HX-Retarget", "#modal-body")
	g.Status(http.StatusUnprocessableEntity)
	c.configForm(g, d)
}

// secretFormError is the secret counterpart of configFormError.
func (c Settings) secretFormError(g *gin.Context, d secretFormData) {
	g.Header("HX-Retarget", "#modal-body")
	g.Status(http.StatusUnprocessableEntity)
	c.secretForm(g, d)
}

// fill completes the page model with the connection state and the
// cluster-scope configs and secrets, keeping any message the caller already set.
func (c Settings) fill(g *gin.Context, d settingsData) settingsData {
	ctx := g.Request.Context()
	apiURL, _, configured := c.loadParams(ctx)
	d.APIURL = apiURL
	d.EnvAPIURL = c.EnvAPI
	d.HasEnvToken = c.EnvToken != ""
	d.Configured = configured
	d.Version = c.Version
	d.User = username(g)
	if u := middleware.CurrentUser(g); u != nil && u.Role == store.RoleAdmin {
		d.CanEditCluster = true
	}
	if _, err := c.Store.GetSetting(ctx, keyToken); err == nil {
		d.HasToken = true
	}
	if configured {
		c.loadCluster(g, &d)
	}
	return d
}

// loadCluster loads the cluster-scope configs and secrets into d. Each list
// keeps its own error pair: one failing call must not blank the other's rows,
// and neither may be mistaken for "nothing configured".
func (c Settings) loadCluster(g *gin.Context, d *settingsData) {
	ctx := g.Request.Context()
	if cfgs, err := c.listConfigsFor(ctx, "cluster", ""); err != nil {
		d.ConfigsErrKey, d.ConfigsErrRaw = "settings.err_config_list", err.Error()
	} else {
		d.ClusterConfigs, d.ConfigsKnown = cfgs, true
	}
	if secs, err := c.listSecretsFor(ctx, "cluster", ""); err != nil {
		d.SecretsErrKey, d.SecretsErrRaw = "settings.err_secret_list", err.Error()
	} else {
		d.ClusterSecrets, d.SecretsKnown = secs, true
	}
}

// renderWith reloads the settings fragment carrying a confirmation message.
func (c Settings) renderWith(g *gin.Context, d settingsData) {
	c.Views.Fragment(g, "settings", c.fill(g, d))
}

// RenderedGet shows one rendered platform config (the YAML after template
// substitution — exactly what is sent to the Swarm) read-only.
func (c Settings) RenderedGet(g *gin.Context) {
	ctx := g.Request.Context()
	name := g.Param("name")
	d := renderedData{Name: name}
	if c.apiRefused(g, &d) {
		c.Views.Fragment(g, "settingsrendered", d)
		return
	}
	rc, err := c.API.ListRenderedConfigs(ctx)
	if err != nil {
		d.fail("settings.err_rendered", err.Error())
		c.Views.Fragment(g, "settingsrendered", d)
		return
	}
	for _, cfg := range rc {
		if cfg.Name == name {
			d.Content = cfg.Content
			c.Views.Fragment(g, "settingsrendered", d)
			return
		}
	}
	// The config row lives in the same table; the snapshot may simply not be
	// recorded yet (fresh install before the first Apply to swarm).
	if _, err := c.API.GetConfig(ctx, name); err == nil {
		d.fail("settings.err_rendered_missing", "")
	} else {
		d.fail("settings.err_rendered_not_found", err.Error())
	}
	c.Views.Fragment(g, "settingsrendered", d)
}

// renderedData drives the read-only rendered-config modal. A missing snapshot is
// its own state, distinct from a broken call: the second carries the daemon's
// raw reply for the disclosure, the first needs none.
type renderedData struct {
	Name    string
	Content string
	ErrKey  string
	ErrRaw  string
	// Error is the pre-i18n prose field. Kept so a caller this file does not own
	// still has somewhere to put a message.
	Error string
}

func (d *renderedData) fail(key, raw string) { d.ErrKey, d.ErrRaw = key, raw }

func username(g *gin.Context) string {
	if u := middleware.CurrentUser(g); u != nil {
		return u.Username
	}
	return ""
}
