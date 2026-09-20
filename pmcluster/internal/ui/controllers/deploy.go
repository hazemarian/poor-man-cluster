package controllers

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

// Deploy exposes the deploy-a-stack form and its submit handler.
type Deploy struct{ *Controller }

// deployData is the view model of frag_deploy.html.
//
// Three-state discipline: StacksKnown separates "the daemon answered with no
// stacks" (an actionable empty list) from "the list call failed" (unknown — the
// page must not draw an empty table for it). The target block does the same for
// the single named stack: named-but-absent is a real state (the daemon will
// create it), and never a revision of 0.
type deployData struct {
	Form        deployForm
	Request     string // the exact JSON body this page posts
	Target      *deployTarget
	TargetNamed bool
	TargetKnown bool
	Stacks      []deployStackRow
	StacksKnown bool
	StacksCount int
	APIReady    bool
	Result      *deployResult
	ErrKey      string
	ErrRaw      string
}

type deployForm struct {
	AppName  string
	Version  string
	RepoURL  string
	Manifest string
}

type deployResult struct {
	Stack    string
	Revision int64
	Changed  bool
}

// deployTarget is the stack this manifest would rewrite: the daemon resolves it
// the same way (app_name wins over the manifest's app field), so the page shows
// what that name currently is instead of guessing.
type deployTarget struct {
	Name      string
	Revision  int64
	RepoURL   string
	UpdatedAt int64
}

// deployStackRow is one row of the stack list, carrying the fields the row's
// actions need — the install id and the current revision are not enough.
type deployStackRow struct {
	Name      string
	Revision  int64
	RepoURL   string
	UpdatedAt int64
}

// Page renders the deploy form, prefilled from the query string so a stack row
// can hand its identity to this form with a plain link.
func (c Deploy) Page(g *gin.Context) {
	d := deployData{Form: deployForm{
		AppName: g.DefaultQuery("app", ""),
		Version: g.DefaultQuery("version", ""),
		RepoURL: g.DefaultQuery("repo", ""),
	}}
	c.load(g, &d)
	c.Views.Fragment(g, "deploy", d)
}

// Submit posts a manifest to the daemon's deploy endpoint and reports the
// revision the daemon stored.
func (c Deploy) Submit(g *gin.Context) {
	ctx := g.Request.Context()
	d := deployData{Form: deployForm{
		AppName:  g.PostForm("app_name"),
		Version:  g.PostForm("version"),
		RepoURL:  g.PostForm("repo_url"),
		Manifest: g.PostForm("manifest"),
	}}

	_, _, configured := c.loadParams(ctx)
	d.APIReady = configured

	switch {
	case !configured:
		d.ErrKey = "err.api_not_configured"
	case strings.TrimSpace(d.Form.Manifest) == "":
		d.ErrKey = "deploy.err_manifest"
	default:
		res, err := c.API.Deploy(ctx, pmapi.DeployPayload{
			AppName:  d.Form.AppName,
			Version:  d.Form.Version,
			RepoURL:  d.Form.RepoURL,
			Manifest: d.Form.Manifest,
		})
		if err != nil {
			d.ErrKey, d.ErrRaw = "deploy.err_deploy", err.Error()
		} else {
			d.Result = &deployResult{Stack: res.Stack, Revision: res.Revision, Changed: res.Changed}
		}
	}

	// Refresh the surrounding context (request preview, target, stack list)
	// around the outcome. A list failure never overwrites the deploy's own
	// error — the more specific one is the one the operator needs.
	c.load(g, &d)
	c.Views.Fragment(g, "deploy", d)
}

// load fills the context zones of the page: the request preview, the target
// stack, and the stack list. Each failure is recorded as its own error key so
// the template can render unknown instead of zero.
func (c Deploy) load(g *gin.Context, d *deployData) {
	ctx := g.Request.Context()
	d.Request = requestBody(d.Form)
	d.TargetNamed = strings.TrimSpace(d.Form.AppName) != ""
	d.APIReady = c.API.Configured()

	if !d.APIReady {
		return
	}

	stacks, err := c.API.ListStacks(ctx)
	if err != nil {
		if d.ErrKey == "" {
			d.ErrKey, d.ErrRaw = "deploy.err_stacks", err.Error()
		}
		return
	}

	d.StacksKnown = true
	d.StacksCount = len(stacks)
	d.Stacks = make([]deployStackRow, 0, len(stacks))
	for _, s := range stacks {
		d.Stacks = append(d.Stacks, deployStackRow{
			Name:      s.Name,
			Revision:  s.CurrentRevision,
			RepoURL:   s.RepoURL,
			UpdatedAt: s.UpdatedAt,
		})
		if d.TargetNamed && s.Name == d.Form.AppName {
			d.Target = &deployTarget{
				Name:      s.Name,
				Revision:  s.CurrentRevision,
				RepoURL:   s.RepoURL,
				UpdatedAt: s.UpdatedAt,
			}
		}
	}
	d.TargetKnown = d.TargetNamed
}

// requestBody renders the exact JSON the console posts. The page shows it so an
// operator can check what the daemon will receive before anything is written —
// the console has no dry-run endpoint, so the body is the review.
func requestBody(f deployForm) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// The manifest is YAML the operator pasted; escaping <, > and & into \u003c
	// would make the preview unreadable without making it safer (the template
	// escapes it again on the way out).
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(pmapi.DeployPayload{
		AppName:  f.AppName,
		Version:  f.Version,
		RepoURL:  f.RepoURL,
		Manifest: f.Manifest,
	}); err != nil {
		return ""
	}
	return buf.String()
}
