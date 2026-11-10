package controllers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	pmsettings "github.com/hazemarian/poor-man-cluster/pmcluster/internal/settings"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/middleware"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
)

// Settings shows and edits the pmcluster API connection, the cluster settings
// and the "Apply to swarm" action. The cluster-scope configs and secrets are
// listed on Inventory → Platform now (one home for both scopes); this page only
// links there, and keeps the create/edit form routes the inventory tab targets.
// Editing a cluster config still re-stamps it with the current binary version,
// so the next "Apply to swarm" (or any cluster up / update) re-renders the
// platform stacks from it.
//
// Two states are load-bearing on this page and are modelled separately:
//
//   - Configured: there is a daemon to ask. When it is false the page says so
//     instead of pretending the platform is healthy.
//   - ApplyDone: this console did trigger a platform update. Without it, an
//     apply that changed nothing would be indistinguishable from no apply.
type Settings struct{ *Controller }

// tokenPlaceholder is what the token field shows when a token is already stored:
// the stored value never travels back to the browser.
const tokenPlaceholder = "•••set•••"

// settingsData is the page model. ErrKey/ErrRaw describe a page-level failure
// (a failed save, a failed apply).
type settingsData struct {
	APIURL      string
	EnvAPIURL   string
	HasToken    bool
	HasEnvToken bool
	Configured  bool
	Version     string
	User        string

	ApplyDone    bool
	ApplyOTel    string
	ApplyTraefik string
	ApplyCert    string
	ApplyEdge    string
	ApplyStacks  string

	// CanEditCluster gates the cluster-settings link: the route is admin-only,
	// and a card that leads to a 403 is worse than no card.
	CanEditCluster bool
	// CanEditGitHub gates the set/rotate form. The route behind it is
	// operator-level, so a viewer sees the card without a form that would 403.
	CanEditGitHub bool

	// GitHub App card. Only ids and the granted permission map travel here;
	// the key and the installation tokens never leave the daemon.
	GitHubConfigured     bool
	GitHubAppID          int64
	GitHubInstallationID int64
	GitHubRepo           string
	GitHubPackage        string
	GitHubPermissions    []githubPermission
	GitHubWarning        string
	GitHubErr            string
	GitHubTestSet        bool
	GitHubTestOK         bool
	GitHubTestMsg        string
	// What is installed right now, shown beside the set/rotate form so an
	// operator can see the key they are about to replace: the fingerprint (never
	// the key) and the last probe, with its age.
	GitHubKeyFingerprint string
	GitHubProbeKnown     bool
	GitHubProbeOK        bool
	GitHubProbeDetail    string
	GitHubProbeAt        int64
	GitHubProbeAgeKey    string
	GitHubProbeAgeArg    string
	// The outcome of a set/rotate submit, rendered directly under the form.
	GitHubSetSet          bool
	GitHubSetAppID        int64
	GitHubSetInstallation int64
	GitHubSetFingerprint  string
	GitHubSetRotated      bool
	GitHubSetKeyUploaded  bool
	GitHubSetProbeOK      bool
	GitHubSetProbeDetail  string

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
	"backup_enabled",
	"backup_cron",
	"backup_retention_days",
	"sso_enabled",
	"sso_provider",
	"sso_client_id",
	"sso_client_secret",
	"sso_github_org",
	"sso_github_repos",
	"sso_cookie_expire",
	"edge_login_disabled",
	"domain",
	"observability_admin_email",
	"traefik_admin_user",
	"github_runners_enabled",
	"github_runner_org",
	"github_runner_group",
	// Nodes are chosen by the picker on the Runners panel; the form carries the
	// current value in a hidden field so saving the form cannot clear it.
	"github_runner_nodes",
	"github_runner_socket_proxy",
	"github_runner_socket_path",
	"github_runner_memory",
	"github_runner_allow_leader",
}

// clusterBoolSettings are the allowlisted settings that are flags rather than
// strings, so the form and the save agree on what a cleared checkbox means.
var clusterBoolSettings = map[string]bool{
	"backup_enabled":             true,
	"sso_enabled":                true,
	"edge_login_disabled":        true,
	"github_runners_enabled":     true,
	"github_runner_socket_proxy": true,
	"github_runner_allow_leader": true,
}

type clusterSettingsData struct {
	Settings        map[string]string
	HasClientSecret bool
	ErrKey          string
	ErrRaw          string
	MsgKey          string
}

// maskClusterSettings masks secret-ish values (sso_client_secret) in the
// rendered form via the shared MaskValue policy: the daemon returns the full
// map (so edits round-trip), but the console only ever shows it masked — never
// the plaintext, and never dropped (the key stays present so the form still
// knows the secret is set).
func maskClusterSettings(settings map[string]string) (map[string]string, bool) {
	if settings == nil {
		return nil, false
	}
	out := make(map[string]string, len(settings))
	for k, v := range settings {
		out[k] = pmsettings.MaskValue(k, v, false)
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
	settings, err := c.API.ClusterSettings(g.Request.Context())
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
	// Retention is the one numeric field on this form, and it decides how much
	// history survives a prune: a typo here that reached the store would either
	// stop pruning (a filled bucket, then failed uploads) or delete history.
	// Empty means "use the default", which is a legitimate choice; anything
	// else has to be a positive whole number.
	if v := strings.TrimSpace(settings["backup_retention_days"]); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 1 {
			d.ErrKey, d.ErrRaw = "settings.err_backup_retention", v
			d.Settings, d.HasClientSecret = maskClusterSettings(settings)
			c.Views.Fragment(g, "clustersettings", d)
			return
		}
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

// githubPermission is one row of the App's granted permission map, prepared for
// the template: the two permissions the cluster needs are marked when missing
// so the card can call them out without the template carrying the rules.
type githubPermission struct {
	Name    string
	Level   string
	Missing bool
}

// fillGitHubApp loads the App registration into the card. A failure is carried
// in GitHubErr (with the raw reply available in the details disclosure) rather
// than failing the whole page: the daemon connection is still worth showing.
func (c *Settings) fillGitHubApp(ctx context.Context, d *settingsData) {
	if !d.Configured {
		return
	}
	st, err := c.API.GitHubAppStatus(ctx)
	if err != nil {
		d.GitHubErr = err.Error()
		return
	}
	d.GitHubConfigured = st.Configured
	d.GitHubAppID = st.AppID
	d.GitHubInstallationID = st.InstallationID
	d.GitHubRepo = st.ProbeRepo
	d.GitHubPackage = st.ProbePackage
	d.GitHubWarning = st.Warning
	d.GitHubKeyFingerprint = st.KeyFingerprint
	d.GitHubProbeAt = st.LastProbeAt
	d.GitHubProbeKnown = st.LastProbeAt > 0
	d.GitHubProbeOK = st.LastProbeOK
	d.GitHubProbeDetail = st.LastProbeDetail
	d.GitHubProbeAgeKey, d.GitHubProbeAgeArg = probeAge(time.Now(), st.LastProbeAt)
	required := map[string]bool{"contents": true, "packages": true}
	names := make([]string, 0, len(st.Permissions))
	for name := range st.Permissions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		level := st.Permissions[name]
		d.GitHubPermissions = append(d.GitHubPermissions, githubPermission{
			Name:  name,
			Level: level,
			// An absent required permission is the case the card exists for:
			// GitHub reports only granted keys, so an absence is the signal.
			Missing: required[name] && permLevel(level) < 1,
		})
	}
}

// permLevel ranks a GitHub permission value the same way the daemon does.
func permLevel(level string) int {
	switch strings.ToLower(level) {
	case "read":
		return 1
	case "write":
		return 2
	case "admin":
		return 3
	}
	return 0
}

// GitHubTest runs the daemon's mint-and-probe on the operator's behalf and
// re-renders the card with the sentence it returned.
func (c Settings) GitHubTest(g *gin.Context) {
	d := settingsData{}
	if c.apiRefused(g, &d) {
		c.Views.Fragment(g, "settings", c.fill(g, d))
		return
	}
	res, err := c.API.TestGitHubApp(g.Request.Context())
	d.GitHubTestSet = true
	if err != nil {
		d.fail("settings.github_err_test", err.Error())
	} else {
		d.GitHubTestOK = res.OK
		d.GitHubTestMsg = res.Message
	}
	c.Views.Fragment(g, "settings", c.fill(g, d))
}

// GitHubAppSet stores or rotates the App registration (app id, installation id,
// private key, probe targets) on the operator's behalf.
//
// Two guards run before the key is parsed:
//   - the route lives in the operator group (a viewer never reaches this);
//   - the browser leg must be TLS or loopback. This handler is the only place a
//     private key crosses from a browser, so the console refuses a cleartext
//     submit here even though its own call to the daemon is loopback HTTP. The
//     daemon enforces the same rule independently for direct API callers.
//
// Nothing is written unless the daemon minted an installation token for the new
// ids with the new key, so a typo in the installation id leaves the working
// registration alone.
func (c Settings) GitHubAppSet(g *gin.Context) {
	ctx := g.Request.Context()
	d := settingsData{}
	if c.apiRefused(g, &d) {
		c.Views.Fragment(g, "settings", c.fill(g, d))
		return
	}
	if !secureBrowserRequest(g.Request) {
		c.githubFormRefused(g, d, "settings.github_err_cleartext", "")
		return
	}

	appID, err := numericForm(g, "app_id")
	if err != nil {
		c.githubFormRefused(g, d, "settings.github_err_bad_id", err.Error())
		return
	}
	installationID, err := numericForm(g, "installation_id")
	if err != nil {
		c.githubFormRefused(g, d, "settings.github_err_bad_id", err.Error())
		return
	}

	keyPEM, err := uploadedKey(g)
	if err != nil {
		c.githubFormRefused(g, d, "settings.github_err_key", err.Error())
		return
	}

	probeRepo := strings.TrimSpace(g.PostForm("probe_repo"))
	probePackage := strings.TrimSpace(g.PostForm("probe_package"))

	// What is installed before this submit, so the answer can say whether the
	// key actually changed.
	var previousFingerprint string
	if st, serr := c.API.GitHubAppStatus(ctx); serr == nil {
		previousFingerprint = st.KeyFingerprint
	}

	proto := ""
	if middleware.IsSecureRequest(g.Request) {
		proto = "https"
	}
	res, err := c.API.SetGitHubApp(ctx, pmapi.GitHubAppSet{
		AppID:          appID,
		InstallationID: installationID,
		PrivateKeyPEM:  string(keyPEM),
		ProbeRepo:      &probeRepo,
		ProbePackage:   &probePackage,
	}, proto)
	if err != nil {
		c.githubFormRefused(g, d, "settings.github_err_set", err.Error())
		return
	}

	d.GitHubSetSet = true
	d.GitHubSetAppID = res.AppID
	d.GitHubSetInstallation = res.InstallationID
	d.GitHubSetFingerprint = res.KeyFingerprint
	d.GitHubSetKeyUploaded = len(keyPEM) > 0
	d.GitHubSetRotated = res.KeyFingerprint != "" && res.KeyFingerprint != previousFingerprint
	d.GitHubSetProbeOK = res.Probe.OK
	d.GitHubSetProbeDetail = res.Probe.Detail

	c.Views.Fragment(g, "settings", c.fill(g, d))
}

// githubFormRefused renders the settings fragment with a failure banner and a
// 400: the form was rejected, and a caller (or a test) should see that in the
// status as well as on the page.
func (c Settings) githubFormRefused(g *gin.Context, d settingsData, key, raw string) {
	d.fail(key, raw)
	g.Status(http.StatusBadRequest)
	c.Views.Fragment(g, "settings", c.fill(g, d))
}

// maxGitHubKeyUpload bounds the .pem the console will read. GitHub's download is
// under 3 KiB and a 4096-bit RSA key under 4 KiB, so 64 KiB is generous for a key
// and still refuses a mis-selected file before it is buffered.
const maxGitHubKeyUpload = 64 << 10

// maxGitHubFormBytes is the whole multipart body: the key plus the other five
// fields and the boundary chrome. Kept just above the key cap so the request is
// refused at the socket rather than after buffering a file.
const maxGitHubFormBytes = maxGitHubKeyUpload + 8<<10

// uploadedKey reads the private key from either input: the file upload wins when
// both are filled (the operator picked a file on purpose), otherwise the pasted
// textarea. An empty result means "keep the stored key".
func uploadedKey(g *gin.Context) ([]byte, error) {
	g.Request.Body = http.MaxBytesReader(g.Writer, g.Request.Body, maxGitHubFormBytes)
	if err := g.Request.ParseMultipartForm(maxGitHubFormBytes); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		return nil, fmt.Errorf("read the form: %w", err)
	}
	file, _, err := g.Request.FormFile("private_key_file")
	switch {
	case err == nil:
		defer func() { _ = file.Close() }()
		data, rerr := io.ReadAll(io.LimitReader(file, maxGitHubKeyUpload+1))
		if rerr != nil {
			return nil, fmt.Errorf("read the uploaded key: %w", rerr)
		}
		if len(data) > maxGitHubKeyUpload {
			return nil, fmt.Errorf("the uploaded key is larger than %d KiB, which is not a PEM", maxGitHubKeyUpload>>10)
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return nil, errors.New("the uploaded file is empty")
		}
		return data, nil
	case errors.Is(err, http.ErrMissingFile):
		// No file chosen: fall through to the textarea.
	default:
		return nil, fmt.Errorf("read the uploaded key: %w", err)
	}
	pasted := g.PostForm("private_key_pem")
	if len(pasted) > maxGitHubKeyUpload {
		return nil, fmt.Errorf("the pasted key is larger than %d KiB, which is not a PEM", maxGitHubKeyUpload>>10)
	}
	return []byte(pasted), nil
}

// numericForm reads an id field. Empty is 0 ("keep what is stored"); anything
// unparseable is named rather than silently treated as absent.
func numericForm(g *gin.Context, field string) (int64, error) {
	raw := strings.TrimSpace(g.PostForm(field))
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number, got %q", field, raw)
	}
	return v, nil
}

// secureBrowserRequest reports whether the browser reached this console over
// TLS, or from this host. The daemon's registration route refuses a private key
// on a cleartext non-loopback hop; this is the same test applied to the leg the
// browser actually used.
func secureBrowserRequest(r *http.Request) bool {
	if middleware.IsSecureRequest(r) {
		return true
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// probeAge turns a probe timestamp into a translation key plus its argument, so
// the card can say "3 min ago" without the template doing date arithmetic.
func probeAge(now time.Time, at int64) (key, arg string) {
	if at <= 0 {
		return "", ""
	}
	age := now.Sub(time.Unix(at, 0))
	switch {
	case age < time.Minute:
		return "settings.github_probe_just_now", ""
	case age < time.Hour:
		return "settings.github_probe_age_min", strconv.Itoa(int(age / time.Minute))
	case age < 24*time.Hour:
		return "settings.github_probe_age_hour", strconv.Itoa(int(age / time.Hour))
	default:
		return "settings.github_probe_age_day", strconv.Itoa(int(age / (24 * time.Hour)))
	}
}

// fill completes the page model with the connection state, the GitHub App
// registration and the cluster-scope pointers, keeping any message the caller
// already set.
func (c Settings) fill(g *gin.Context, d settingsData) settingsData {
	ctx := g.Request.Context()
	apiURL, _, configured := c.loadParams(ctx)
	d.APIURL = apiURL
	d.EnvAPIURL = c.EnvAPI
	d.HasEnvToken = c.EnvToken != ""
	d.Configured = configured
	d.Version = c.Version
	d.User = username(g)
	if u := middleware.CurrentUser(g); u != nil {
		if u.Role == store.RoleAdmin {
			d.CanEditCluster = true
		}
		// operator or admin: the daemon gates the registration write at operator
		// level, so the form is offered to exactly the roles the route accepts.
		if u.Role == store.RoleAdmin || u.Role == store.RoleOperator {
			d.CanEditGitHub = true
		}
	}
	if _, err := c.Store.Setting(ctx, keyToken); err == nil {
		d.HasToken = true
	}
	c.fillGitHubApp(ctx, &d)
	return d
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
	if _, err := c.API.Config(ctx, name); err == nil {
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

// stackPickerData backs the small "which stack?" modal that sits in front of the
// service-scope config/secret forms.
//
// It exists because of a gap in the shape of the console, not in the API: the
// only place that offered "new secret for a stack" was the stack's own page
// (/web/stacks/{name}/config), and that page is reachable only once the stack
// exists in the store. But a manifest references config(<name>) and
// secret(<name>) from its very FIRST deploy — a first deploy that fails because
// those are missing, so the stack never comes into being, so the page is never
// reachable, so they can never be created. The API and the CLI never had this
// problem: POST /api/secrets takes any stack name.
type stackPickerData struct {
	// Kind is "secret" or "config" — it selects the follow-up form and the copy.
	Kind string
	// Action is the HTMX endpoint the picker submits to; it re-renders the modal
	// with the real form filled in with this stack name.
	Action string
	// Stacks are suggestions from the cluster: deployed stacks plus any stack
	// that already owns a service-scope config or secret (a stack can be
	// pre-provisioned before its first deploy, so those matter too).
	Stacks []string
	// ErrKey/ErrRaw render "the API did not answer" instead of an empty list.
	ErrKey string
	ErrRaw string
}

func (d *stackPickerData) fail(key, raw string) { d.ErrKey, d.ErrRaw = key, raw }

// SecretForStack renders the service-scope secret form for a typed stack name.
// With no ?stack= it renders the picker first, so the operator sees what exists.
func (c Settings) SecretForStack(g *gin.Context) {
	stack := strings.TrimSpace(g.Query("stack"))
	if stack == "" {
		c.stackPicker(g, "secret", "/web/settings/secrets/for-stack")
		return
	}
	d := secretFormData{
		Scope:  "service",
		Stack:  stack,
		Action: WebBase + "/stacks/" + url.PathEscape(stack) + "/secrets/add",
	}
	if c.apiRefused(g, &d) {
		c.secretFormError(g, d)
		return
	}
	c.secretForm(g, d)
}

// ConfigForStack is the same entry point for service-scope configs.
func (c Settings) ConfigForStack(g *gin.Context) {
	stack := strings.TrimSpace(g.Query("stack"))
	if stack == "" {
		c.stackPicker(g, "config", "/web/settings/configs/for-stack")
		return
	}
	d := configFormData{
		Scope:  "service",
		Stack:  stack,
		Kind:   "file",
		Action: WebBase + "/stacks/" + url.PathEscape(stack) + "/configs/add",
	}
	if c.apiRefused(g, &d) {
		c.configFormError(g, d)
		return
	}
	c.configForm(g, d)
}

// stackPicker renders the picker with the stack names the cluster already knows.
// A failure to list them is not fatal — the field is free text, so the operator
// can always type the name of a stack that does not exist yet, which is the case
// this whole flow is for.
func (c Settings) stackPicker(g *gin.Context, kind, action string) {
	d := stackPickerData{Kind: kind, Action: action}
	ctx := g.Request.Context()
	seen := map[string]bool{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name != "" && !seen[name] {
			seen[name] = true
			d.Stacks = append(d.Stacks, name)
		}
	}
	if stacks, err := c.API.ListStacks(ctx); err == nil {
		for _, s := range stacks {
			add(s.Name)
		}
	}
	// Stacks that only exist as the owner of a service-scope item: a stack can be
	// pre-provisioned before its first deploy, and suggesting it is the point.
	for _, scope := range []string{"service"} {
		if cfgs, err := c.API.ListConfigs(ctx, scope, ""); err == nil {
			for _, cf := range cfgs {
				add(cf.Stack)
			}
		}
		if secs, err := c.API.ListSecrets(ctx, scope, ""); err == nil {
			for _, s := range secs {
				add(s.Stack)
			}
		}
	}
	sort.Strings(d.Stacks)
	c.Views.Fragment(g, "stackpicker", d)
}
