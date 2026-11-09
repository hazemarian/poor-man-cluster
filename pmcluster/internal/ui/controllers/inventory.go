package controllers

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// Inventory is the one page where both scopes meet: the Stacks tab lists every
// stack's service-scope configs and secrets, the Platform tab lists the
// cluster-scope rows. Each tab has its own server-side name search, and the
// active tab rides in the query (?tab=platform&q=) so a search, a refresh or a
// deep link lands on the same view.
//
// The stack page (StackConfigs) stays the focused view for one stack and never
// shows cluster-scope rows; this page is the "show me everything" surface.
type Inventory struct{ *Controller }

// invConfigPanel is one configs table: its rows, its unfiltered total (so the
// page can say "3 of 12"), and the scope-specific copy and create target that
// the shared panel template renders. Known/ErrKey keep a failed read distinct
// from an empty list, as on the stack page.
type invConfigPanel struct {
	TitleKey string
	Rows     []configRow
	Total    int
	Known    bool
	ErrKey   string
	ErrRaw   string

	Filtered  bool
	Query     string
	Tab       string
	Scope     string // service | cluster
	ShowStack bool

	EmptyTitleKey string
	EmptyBodyKey  string
	CreateURL     string
	CreateTipKey  string
}

// invSecretPanel is the secrets counterpart of invConfigPanel.
type invSecretPanel struct {
	TitleKey string
	Rows     []secretRow
	Total    int
	Known    bool
	ErrKey   string
	ErrRaw   string

	Filtered  bool
	Query     string
	Tab       string
	Scope     string
	ShowStack bool

	EmptyTitleKey string
	EmptyBodyKey  string
	CreateURL     string
	CreateTipKey  string
}

// inventoryData is the page model. Both tabs are loaded even though only the
// active one renders: the count pill and the tab's own state must be right the
// moment the operator switches, without a second read.
type inventoryData struct {
	Tab      string
	Query    string
	Filtered bool

	StackConfigs    invConfigPanel
	StackSecrets    invSecretPanel
	PlatformConfigs invConfigPanel
	PlatformSecrets invSecretPanel

	// MsgKey/MsgArg carry the result of an action (delete) that re-rendered
	// this fragment; MsgErr picks the banner's tone.
	MsgKey string
	MsgArg string
	MsgErr bool

	ErrKey string
	ErrRaw string
}

func (d *inventoryData) fail(key, raw string) { d.ErrKey, d.ErrRaw = key, raw }

// Page renders the inventory fragment for the requested tab and search.
func (c Inventory) Page(g *gin.Context) {
	c.render(g, inventoryData{Tab: g.Query("tab"), Query: g.Query("q")})
}

// normalizeInventoryTab clamps the tab to the two this page has; anything else
// (including an empty query, a refresh, a deep link) is the Stacks tab.
func normalizeInventoryTab(tab string) string {
	if tab == "platform" {
		return "platform"
	}
	return "stacks"
}

// render loads both scopes and renders the fragment. The daemon is asked once
// per kind (all scopes, all stacks) and the split happens here: a row's scope
// decides its tab, so a cluster-scope row can never appear in the Stacks tab.
func (c Inventory) render(g *gin.Context, d inventoryData) {
	d.Tab = normalizeInventoryTab(d.Tab)
	d.Query = strings.TrimSpace(d.Query)
	d.Filtered = d.Query != ""

	stackCfg, stackSec := c.newServicePanels(d)
	platformCfg, platformSec := c.newClusterPanels(d)

	if c.apiRefused(g, &d) {
		c.Views.Fragment(g, "inventory", d)
		return
	}
	ctx := g.Request.Context()

	cfgs, cfgErr := c.listConfigsFor(ctx, "", "")
	secs, secErr := c.listSecretsFor(ctx, "", "")

	// Annotate every row with the stacks that reference it (usage graph).
	// Best-effort: a failed usage call leaves the page readable without the
	// column rather than failing the page.
	if cfgErr == nil {
		if u, err := c.API.Usage(ctx); err == nil {
			for i := range cfgs {
				cfgs[i].References = u.Configs[cfgs[i].Name]
			}
			for i := range secs {
				secs[i].References = u.Secrets[secs[i].Name]
			}
		}
	}

	fillPanels(&stackCfg, &platformCfg, cfgs, d.Filtered, d.Query, cfgErr)
	fillSecretPanels(&stackSec, &platformSec, secs, d.Filtered, d.Query, secErr)

	d.StackConfigs, d.StackSecrets = stackCfg, stackSec
	d.PlatformConfigs, d.PlatformSecrets = platformCfg, platformSec
	c.Views.Fragment(g, "inventory", d)
}

// newServicePanels describes the Stacks tab. Create targets are the existing
// service-scope entry points: the stack picker in front of the stack's form.
func (c Inventory) newServicePanels(d inventoryData) (invConfigPanel, invSecretPanel) {
	cfg := invConfigPanel{
		TitleKey:      "inventory.configs_title",
		Filtered:      d.Filtered,
		Query:         d.Query,
		Tab:           d.Tab,
		Scope:         "service",
		ShowStack:     true,
		EmptyTitleKey: "inventory.stack_configs_empty_title",
		EmptyBodyKey:  "inventory.stack_configs_empty_body",
		CreateURL:     WebBase + "/settings/configs/for-stack",
		CreateTipKey:  "inventory.form_new_config",
	}
	sec := invSecretPanel{
		TitleKey:      "inventory.secrets_title",
		Filtered:      d.Filtered,
		Query:         d.Query,
		Tab:           d.Tab,
		Scope:         "service",
		ShowStack:     true,
		EmptyTitleKey: "inventory.stack_secrets_empty_title",
		EmptyBodyKey:  "inventory.stack_secrets_empty_body",
		CreateURL:     WebBase + "/settings/secrets/for-stack",
		CreateTipKey:  "inventory.form_new_secret",
	}
	return cfg, sec
}

// newClusterPanels describes the Platform tab. Create targets are the existing
// cluster-scope forms, which stay exactly where they were.
func (c Inventory) newClusterPanels(d inventoryData) (invConfigPanel, invSecretPanel) {
	cfg := invConfigPanel{
		TitleKey:      "inventory.configs_title",
		Filtered:      d.Filtered,
		Query:         d.Query,
		Tab:           d.Tab,
		Scope:         "cluster",
		EmptyTitleKey: "inventory.configs_empty_title",
		EmptyBodyKey:  "inventory.configs_empty_body",
		CreateURL:     WebBase + "/settings/configs/new",
		CreateTipKey:  "inventory.form_new_config",
	}
	sec := invSecretPanel{
		TitleKey:      "inventory.secrets_title",
		Filtered:      d.Filtered,
		Query:         d.Query,
		Tab:           d.Tab,
		Scope:         "cluster",
		EmptyTitleKey: "inventory.secrets_empty_title",
		EmptyBodyKey:  "inventory.secrets_empty_body",
		CreateURL:     WebBase + "/settings/secrets/new",
		CreateTipKey:  "inventory.form_new_secret",
	}
	return cfg, sec
}

// fillPanels splits the loaded rows by scope and applies the search to each
// tab independently. Total is measured before the search so the count reads
// "n of total"; a failed read marks both panels unknown instead of empty.
func fillPanels(stack, platform *invConfigPanel, cfgs []configRow, filtered bool, query string, err error) {
	if err != nil {
		stack.ErrKey, stack.ErrRaw = "inventory.err_configs", err.Error()
		platform.ErrKey, platform.ErrRaw = "inventory.err_configs", err.Error()
		return
	}
	stack.Known, platform.Known = true, true
	for _, row := range cfgs {
		if row.Scope == "cluster" {
			platform.Rows = append(platform.Rows, row)
		} else {
			stack.Rows = append(stack.Rows, row)
		}
	}
	stack.Total, platform.Total = len(stack.Rows), len(platform.Rows)
	if filtered {
		stack.Rows = filterConfigRows(stack.Rows, query)
		platform.Rows = filterConfigRows(platform.Rows, query)
	}
}

// fillSecretPanels is fillPanels for secrets.
func fillSecretPanels(stack, platform *invSecretPanel, secs []secretRow, filtered bool, query string, err error) {
	if err != nil {
		stack.ErrKey, stack.ErrRaw = "inventory.err_secrets", err.Error()
		platform.ErrKey, platform.ErrRaw = "inventory.err_secrets", err.Error()
		return
	}
	stack.Known, platform.Known = true, true
	for _, row := range secs {
		if row.Scope == "cluster" {
			platform.Rows = append(platform.Rows, row)
		} else {
			stack.Rows = append(stack.Rows, row)
		}
	}
	stack.Total, platform.Total = len(stack.Rows), len(platform.Rows)
	if filtered {
		stack.Rows = filterSecretRows(stack.Rows, query)
		platform.Rows = filterSecretRows(platform.Rows, query)
	}
}

// filterConfigRows keeps rows whose name contains q, case-insensitively.
// Names are the only searchable field: a secret's value never leaves the
// daemon, so there is nothing else to match against.
func filterConfigRows(rows []configRow, q string) []configRow {
	q = strings.ToLower(q)
	out := make([]configRow, 0, len(rows))
	for _, row := range rows {
		if strings.Contains(strings.ToLower(row.Name), q) {
			out = append(out, row)
		}
	}
	return out
}

func filterSecretRows(rows []secretRow, q string) []secretRow {
	q = strings.ToLower(q)
	out := make([]secretRow, 0, len(rows))
	for _, row := range rows {
		if strings.Contains(strings.ToLower(row.Name), q) {
			out = append(out, row)
		}
	}
	return out
}

// Remove deletes a config or secret from either scope and re-renders the
// inventory fragment on the tab (and search) the operator was looking at.
// Rows from both scopes are managed here now that the Settings page only links
// to this one.
func (c Inventory) Remove(g *gin.Context) {
	kind, name := g.Param("kind"), g.Param("name")
	d := inventoryData{Tab: g.PostForm("tab"), Query: g.PostForm("q")}
	ctx := g.Request.Context()

	var err error
	switch kind {
	case "config":
		err = c.API.DeleteConfig(ctx, name)
	case "secret":
		err = c.API.DeleteSecret(ctx, name)
	default:
		d.MsgKey, d.MsgArg, d.MsgErr = "inventory.err_remove", name, true
		c.render(g, d)
		return
	}
	switch {
	case err != nil:
		d.MsgKey, d.MsgArg, d.MsgErr = "err."+kind+"_remove", name, true
	case kind == "config":
		d.MsgKey, d.MsgArg = "inventory.msg_config_removed", name
	default:
		d.MsgKey, d.MsgArg = "inventory.msg_secret_removed", name
	}
	c.render(g, d)
}

// retagFormData is the scope/stack move form model. Kind identifies whether the
// row is a config or a secret — both move through the same modal form. Tab is
// carried so a successful move returns to the tab it started from.
type retagFormData struct {
	Kind   string // "config" or "secret"
	Name   string
	Scope  string
	Stack  string
	Action string
	Error  string
	Tab    string
}

// RetagForm renders the "move to a different scope/stack" form into the modal.
// It fixes rows created without a stack tag (stack "") from the console
// instead of forcing delete + recreate.
func (c Inventory) RetagForm(g *gin.Context) {
	kind, name := g.Param("kind"), g.Param("name")
	scope := g.Query("scope")
	if scope == "" {
		scope = "service"
	}
	d := retagFormData{
		Kind: kind, Name: name, Scope: scope,
		Action: WebBase + "/inventory/retag",
		Tab:    normalizeInventoryTab(g.Query("tab")),
	}
	c.Views.Fragment(g, "retagform", d)
}

// Retag moves a config or secret to a new scope/stack and re-renders the
// inventory fragment on the originating tab. Errors keep the modal open with
// the input intact.
func (c Inventory) Retag(g *gin.Context) {
	kind, name := g.PostForm("kind"), g.PostForm("name")
	scope, stack := g.PostForm("scope"), g.PostForm("stack")
	d := retagFormData{
		Kind: kind, Name: name, Scope: scope, Stack: stack,
		Action: WebBase + "/inventory/retag",
		Tab:    normalizeInventoryTab(g.PostForm("tab")),
	}
	ctx := g.Request.Context()
	var err error
	switch kind {
	case "config":
		err = c.API.RetagConfig(ctx, name, scope, stack)
	case "secret":
		err = c.API.RetagSecret(ctx, name, scope, stack)
	default:
		d.Error = "unknown row kind " + kind
	}
	if err != nil {
		d.Error = err.Error()
		c.Views.Fragment(g, "retagform", d)
		return
	}
	c.render(g, inventoryData{Tab: d.Tab})
}
