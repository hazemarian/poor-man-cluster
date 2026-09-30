package controllers

import (
	"github.com/gin-gonic/gin"
)

// Inventory shows every stored config and secret across all scopes and stacks
// — the platform's cluster-scope rows, each stack's service-scope rows, and the
// unattached service rows (stack "") shared by any stack that references their
// name. The per-stack page (StackConfigs) stays the focused view: it lists the
// stack's own rows plus the shared ones. This page is the "show me everything"
// surface, with Scope and Stack columns making ownership explicit.
type Inventory struct{ *Controller }

// inventoryData is the page model. Each list keeps its own known/error pair so
// a failed call is never rendered as "nothing exists".
type inventoryData struct {
	Configs       []configRow
	ConfigsKnown  bool
	ConfigsErrKey string
	ConfigsErrRaw string

	Secrets       []secretRow
	SecretsKnown  bool
	SecretsErrKey string
	SecretsErrRaw string

	ErrKey string
	ErrRaw string
}

func (d *inventoryData) fail(key, raw string) { d.ErrKey, d.ErrRaw = key, raw }

// Page renders the inventory fragment: every config and every secret, each
// annotated with the stacks that reference it (from the usage graph) so a
// delete or retag visibly shows what would break.
func (c Inventory) Page(g *gin.Context) {
	d := inventoryData{}
	if c.apiRefused(g, &d) {
		c.Views.Fragment(g, "inventory", d)
		return
	}
	ctx := g.Request.Context()
	if cfgs, err := c.listConfigsFor(ctx, "", ""); err != nil {
		d.ConfigsErrKey, d.ConfigsErrRaw = "inventory.err_configs", err.Error()
	} else {
		d.Configs, d.ConfigsKnown = cfgs, true
	}
	if secs, err := c.listSecretsFor(ctx, "", ""); err != nil {
		d.SecretsErrKey, d.SecretsErrRaw = "inventory.err_secrets", err.Error()
	} else {
		d.Secrets, d.SecretsKnown = secs, true
	}
	// Annotate every row with the stacks that reference it. Best-effort: a
	// failed usage call leaves the page readable without the column.
	if u, err := c.API.GetUsage(ctx); err == nil && d.ConfigsKnown {
		for i := range d.Configs {
			d.Configs[i].References = u.Configs[d.Configs[i].Name]
		}
		for i := range d.Secrets {
			d.Secrets[i].References = u.Secrets[d.Secrets[i].Name]
		}
	}
	c.Views.Fragment(g, "inventory", d)
}

// retagFormData is the scope/stack move form model. Kind identifies whether the
// row is a config or a secret — both move through the same modal form.
type retagFormData struct {
	Kind   string // "config" or "secret"
	Name   string
	Scope  string
	Stack  string
	Action string
	Error  string
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
	}
	c.Views.Fragment(g, "retagform", d)
}

// Retag moves a config or secret to a new scope/stack and re-renders the
// inventory fragment. Errors keep the modal open with the input intact.
func (c Inventory) Retag(g *gin.Context) {
	kind, name := g.PostForm("kind"), g.PostForm("name")
	scope, stack := g.PostForm("scope"), g.PostForm("stack")
	d := retagFormData{
		Kind: kind, Name: name, Scope: scope, Stack: stack,
		Action: WebBase + "/inventory/retag",
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
	c.Page(g)
}
