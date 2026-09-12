package controllers

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// Stacks lists and inspects stacks; StacksOps performs the write operations.
type Stacks struct{ *Controller }

type stackData struct {
	Stacks []stackRow
	Count  int
	Error  string
	Msg    string
}

type stackRow struct {
	Name            string
	CurrentRevision int64
	RepoURL         string
	UpdatedAt       int64
}

// List renders the stacks table.
func (c Stacks) List(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := stackData{}
	if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
		c.Views.Fragment(g, "stacks", d)
		return
	}
	stacks, err := c.API.ListStacks(ctx)
	if err != nil {
		d.Error = err.Error()
		c.Views.Fragment(g, "stacks", d)
		return
	}
	for _, s := range stacks {
		d.Stacks = append(d.Stacks, stackRow{
			Name: s.Name, CurrentRevision: s.CurrentRevision,
			RepoURL: s.RepoURL, UpdatedAt: s.UpdatedAt,
		})
	}
	d.Count = len(d.Stacks)
	c.Views.Fragment(g, "stacks", d)
}

type stackDetailData struct {
	Detail *stackDetail
	Error  string
	Msg    string
}

type stackDetail struct {
	Stack      stackRow
	Revisions  []revRow
	LastBackup *lastBackup
}

type revRow struct {
	Revision  int64
	CreatedAt int64
}

type lastBackup struct {
	Status    string
	StartedAt int64
}

// Show renders one stack's detail with its revisions.
func (c Stacks) Show(g *gin.Context) {
	name := g.Param("name")
	d := stackDetailData{Msg: g.Query("msg")}
	c.renderStack(g, name, d)
}

// ShowRevision renders a single revision's source + rendered manifests.
func (c Stacks) ShowRevision(g *gin.Context) {
	ctx := g.Request.Context()
	name := g.Param("name")
	rev, err := strconv.ParseInt(g.Param("rev"), 10, 64)
	if err != nil {
		c.Views.Fragment(g, "revision", gin.H{"Error": "invalid revision: " + err.Error()})
		return
	}
	c.loadParams(ctx)
	rv, err := c.API.GetRevision(ctx, name, rev)
	if err != nil {
		c.Views.Fragment(g, "revision", gin.H{"Error": err.Error()})
		return
	}
	c.Views.Fragment(g, "revision", gin.H{"Rev": rv})
}

// renderStack loads stack detail and renders the fragment (with optional Msg).
func (c Stacks) renderStack(g *gin.Context, name string, base stackDetailData) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	if !configured {
		base.Error = "pmcluster API not configured. Open Settings first."
		c.Views.Fragment(g, "stack", base)
		return
	}
	det, err := c.API.GetStack(ctx, name)
	if err != nil {
		base.Error = err.Error()
		c.Views.Fragment(g, "stack", base)
		return
	}
	sd := &stackDetail{
		Stack: stackRow{
			Name: det.Stack.Name, CurrentRevision: det.Stack.CurrentRevision,
			RepoURL: det.Stack.RepoURL, UpdatedAt: det.Stack.UpdatedAt,
		},
	}
	for _, r := range det.Revisions {
		sd.Revisions = append(sd.Revisions, revRow{Revision: r.Revision, CreatedAt: r.CreatedAt})
	}
	if det.LastBackup != nil {
		sd.LastBackup = &lastBackup{Status: det.LastBackup.Status, StartedAt: det.LastBackup.StartedAt}
	}
	base.Detail = sd
	c.Views.Fragment(g, "stack", base)
}

// Rollback reverts a stack to a prior revision, then re-renders its detail.
func (c Stacks) Rollback(g *gin.Context) {
	ctx := g.Request.Context()
	name := g.Param("name")
	rev, err := strconv.ParseInt(g.PostForm("revision"), 10, 64)
	if err != nil {
		c.renderStack(g, name, stackDetailData{Error: "revision must be an integer"})
		return
	}
	c.loadParams(ctx)
	_, err = c.API.Rollback(ctx, name, rev)
	msg := "Rolled back " + name + " to revision " + strconv.FormatInt(rev, 10)
	if err != nil {
		msg = ""
	}
	c.renderStack(g, name, stackDetailData{Msg: msg, Error: errStringIf(err)})
}

// ShowBackups renders backups scoped to one stack.
func (c Stacks) ShowBackups(g *gin.Context) {
	ctx := g.Request.Context()
	name := g.Param("name")
	c.loadParams(ctx)
	rows, err := c.API.ListStackBackups(ctx, name)
	d := backupsData{Name: name}
	if err != nil {
		d.Error = err.Error()
		c.Views.Fragment(g, "backups", d)
		return
	}
	for _, b := range rows {
		d.Backups = append(d.Backups, backupRowFrom(b))
	}
	c.Views.Fragment(g, "backups", d)
}

func errStringIf(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
