package controllers

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// Stacks lists stacks, inspects one stack, and performs the stack-level writes
// the console offers (redeploy, roll back, delete).
//
// Three answers are kept apart everywhere in here: a value we read, "unknown"
// (the API did not answer), and "empty" (the API answered with nothing). The
// v1 page collapsed the first two into an empty table, which is why Known,
// ServicesKnown and the ErrKey fields exist.
//
// Every handler answers with the whole index, inspector included, and no
// handler pushes the POST URL into history: the operator keeps the list and
// the open stack across an action, and a refresh lands on /stacks/{name}.
type Stacks struct{ *Controller }

// stackInfo identifies a stack in the table and in the inspector.
type stackInfo struct {
	Name            string
	CurrentRevision int64
	RepoURL         string
	CreatedAt       int64
	UpdatedAt       int64
}

// stackRow is one line of the stacks table. The replica counts come from the
// swarm (GET /api/services), never from the stack record: a stack whose
// services were removed by hand still has a revision but runs nothing.
type stackRow struct {
	stackInfo
	Services int
	Running  int64
	Desired  int64
}

// State classifies a row by whether the swarm runs what the stack asks for.
// Callers must check ServicesKnown first: an unread service list is not the
// same answer as zero services.
func (r stackRow) State() string {
	switch {
	case r.Services == 0:
		return "none"
	case r.Desired > 0 && r.Running >= r.Desired:
		return "running"
	default:
		return "degraded"
	}
}

// stackData is the stacks index plus the optional open inspector.
type stackData struct {
	Stacks []stackRow

	// Count is the number of rows shown, Total the number that exist: a
	// filtered view must not claim the cluster has fewer stacks than it has.
	Count         int
	Total         int
	RepoCount     int
	TotalServices int
	TotalRunning  int64
	TotalDesired  int64
	LastUpdated   int64

	// Query is the operator's filter, Filtered whether it is in force.
	Query    string
	Filtered bool

	// Known is false when ListStacks failed; ServicesKnown is false when the
	// swarm was not read, so every replica cell stays "unavailable".
	Known         bool
	ServicesKnown bool

	// Open is the inspector, already loaded, or nil when closed. It is part of
	// the index render so an action, a deep link and a refresh all show the
	// same page instead of a drawer floating over an empty view.
	Open *stackDetailData

	ErrKey string
	ErrRaw string

	MsgKey  string
	MsgArg0 string
	MsgArg1 string
	MsgArg2 string
}

// stackDetail is one stack's read-only detail: what the inspector shows.
type stackDetail struct {
	Stack      stackInfo
	Revisions  []revRow
	LastBackup *backupInfo
}

// revRow is one entry of the revision history.
type revRow struct {
	Revision  int64
	CreatedAt int64
	Current   bool
}

// backupInfo is the last backup of a stack, with the pill tone resolved here
// so the template carries no status vocabulary.
type backupInfo struct {
	Status       string
	Tone         string
	StartedAt    int64
	FinishedAt   int64
	Revision     int64
	ErrorMessage string
}

// stackDetailData is the inspector. It carries the stack name even when the
// read failed, so the failed state can still say which stack it is about.
type stackDetailData struct {
	Name   string
	Detail *stackDetail

	ErrKey string
	ErrRaw string
}

// revisionData is one revision's manifests, rendered in the modal.
type revisionData struct {
	Stack    string
	Revision int64
	Created  int64
	Source   string
	Rendered string

	ErrKey string
	ErrRaw string
}

// List renders the stacks table, with one stack's inspector open when the URL
// asks for it (?open=name).
func (c Stacks) List(g *gin.Context) {
	d := c.stacksData(g.Request.Context(), g.Query("q"))
	c.renderIndex(g, d, g.Query("open"))
}

// Show serves /stacks/{name}. It renders the whole index with that stack open,
// so a pasted or refreshed URL keeps the table it was opened from.
func (c Stacks) Show(g *gin.Context) {
	d := c.stacksData(g.Request.Context(), g.Query("q"))
	c.renderIndex(g, d, g.Param("name"))
}

// Remove deletes a stack and re-renders the index it was deleted from. The
// inspector stays closed: the stack it described no longer exists.
func (c Stacks) Remove(g *gin.Context) {
	ctx := g.Request.Context()
	name := g.Param("name")

	msgKey, msgArg, errKey, errRaw := "", "", "", ""
	if _, _, configured := c.loadParams(ctx); !configured {
		errKey = "err.api_not_configured"
	} else if err := c.API.DeleteStack(ctx, name); err != nil {
		errKey, errRaw = "err.stack_remove", err.Error()
	} else {
		msgKey, msgArg = "stacks.msg_removed", name
	}

	// The table is re-read either way: after a delete the row must be gone,
	// and after a failure the operator still needs the list he was looking at.
	d := c.stacksData(ctx, g.Query("q"))
	if errKey != "" {
		d.ErrKey, d.ErrRaw = errKey, errRaw
	} else {
		d.MsgKey, d.MsgArg0 = msgKey, msgArg
	}
	open := ""
	if errKey != "" {
		// The delete failed, so the stack is still there: leave the operator
		// in the inspector he acted from instead of bouncing him to the list.
		open = name
	}
	c.renderIndex(g, d, open)
}

// Sync (re)deploys a stack from its newest stored manifest.
func (c Stacks) Sync(g *gin.Context) {
	ctx := g.Request.Context()
	name := g.Param("name")
	d := c.stacksData(ctx, g.Query("q"))

	if _, _, configured := c.loadParams(ctx); !configured {
		d.ErrKey = "err.api_not_configured"
	} else if res, err := c.API.SyncStack(ctx, name); err != nil {
		d.ErrKey, d.ErrRaw = "err.stack_sync", err.Error()
	} else if res != nil && res.Changed {
		d.MsgKey, d.MsgArg0 = "stack.msg_synced", name
		d.MsgArg1 = strconv.FormatInt(res.Revision, 10)
	} else {
		d.MsgKey, d.MsgArg0 = "stack.msg_synced_same", name
	}
	c.renderIndex(g, d, name)
}

// Rollback redeploys a stack from an older revision's manifest. The API writes
// the result as a new revision, so the message reports both numbers rather
// than implying the old revision is live again.
func (c Stacks) Rollback(g *gin.Context) {
	ctx := g.Request.Context()
	name := g.Param("name")
	d := c.stacksData(ctx, g.Query("q"))

	rev, err := strconv.ParseInt(strings.TrimSpace(g.PostForm("revision")), 10, 64)
	switch {
	case err != nil || rev <= 0:
		d.ErrKey = "stack.err_bad_revision"
	case func() bool { _, _, ok := c.loadParams(ctx); return !ok }():
		d.ErrKey = "err.api_not_configured"
	default:
		res, err := c.API.Rollback(ctx, name, rev)
		if err != nil {
			d.ErrKey, d.ErrRaw = "err.stack_rollback", err.Error()
		} else {
			to, now := rev, rev
			if res != nil {
				to, now = res.RolledBackTo, res.NewRevision
			}
			d.MsgKey = "stack.msg_rolled_back"
			d.MsgArg0 = name
			d.MsgArg1 = strconv.FormatInt(to, 10)
			d.MsgArg2 = strconv.FormatInt(now, 10)
		}
	}
	c.renderIndex(g, d, name)
}

// ShowRevision renders one revision's source and rendered manifests. It is
// loaded into the modal body, and is also reachable standalone at
// /stacks/{name}/revisions/{rev}.
func (c Stacks) ShowRevision(g *gin.Context) {
	ctx := g.Request.Context()
	name := g.Param("name")
	d := revisionData{Stack: name}

	if _, _, configured := c.loadParams(ctx); !configured {
		d.ErrKey = "err.api_not_configured"
		c.Views.Fragment(g, "revision", d)
		return
	}

	rev, err := strconv.ParseInt(strings.TrimSpace(g.Param("rev")), 10, 64)
	if err != nil || rev <= 0 {
		d.ErrKey = "revision.err_number"
		c.Views.Fragment(g, "revision", d)
		return
	}
	d.Revision = rev

	rv, err := c.API.GetRevision(ctx, name, rev)
	if err != nil {
		d.ErrKey, d.ErrRaw = "err.revision", err.Error()
		c.Views.Fragment(g, "revision", d)
		return
	}
	d.Stack, d.Revision = rv.Stack, rv.Revision
	d.Created = rv.CreatedAt
	d.Source, d.Rendered = rv.SourceYAML, rv.RenderedYAML
	c.Views.Fragment(g, "revision", d)
}

// renderIndex draws the stacks page, with the inspector for open loaded when a
// name is given. An unread inspector is rendered as an unknown state rather
// than as a stack with nothing in it.
func (c Stacks) renderIndex(g *gin.Context, d stackData, open string) {
	if name := strings.TrimSpace(open); name != "" {
		sd := c.loadStack(g.Request.Context(), name)
		d.Open = &sd
	}
	c.Views.Fragment(g, "stacks", d)
}

// stacksData builds the index: the stack list, the swarm's replica counts, and
// the optional name/revision/repository filter.
func (c Stacks) stacksData(ctx context.Context, q string) stackData {
	q = strings.TrimSpace(q)
	d := stackData{Query: q, Filtered: q != ""}

	if _, _, configured := c.loadParams(ctx); !configured {
		d.ErrKey = "err.api_not_configured"
		return d
	}

	stacks, err := c.API.ListStacks(ctx)
	if err != nil {
		d.ErrKey, d.ErrRaw = "err.stacks", err.Error()
		return d
	}
	d.Known = true

	rows := make([]stackRow, 0, len(stacks))
	for _, s := range stacks {
		rows = append(rows, stackRow{stackInfo: stackInfo{
			Name:            s.Name,
			CurrentRevision: s.CurrentRevision,
			RepoURL:         s.RepoURL,
			CreatedAt:       s.CreatedAt,
			UpdatedAt:       s.UpdatedAt,
		}})
	}
	byName := make(map[string]*stackRow, len(rows))
	for i := range rows {
		byName[rows[i].Name] = &rows[i]
		if rows[i].RepoURL != "" {
			d.RepoCount++
		}
		if rows[i].UpdatedAt > d.LastUpdated {
			d.LastUpdated = rows[i].UpdatedAt
		}
	}

	// One request covers every stack. If it fails the replica cells stay
	// "unavailable" instead of claiming 0/0.
	if svcs, err := c.API.ListServices(ctx, ""); err == nil {
		d.ServicesKnown = true
		for _, s := range svcs {
			d.TotalServices++
			d.TotalRunning += int64(s.Replicas)
			d.TotalDesired += int64(s.Desired)
			if r, ok := byName[s.Stack]; ok {
				r.Services++
				r.Running += int64(s.Replicas)
				r.Desired += int64(s.Desired)
			}
		}
	}

	d.Total = len(rows)
	if !d.Filtered {
		d.Stacks = rows
		d.Count = len(rows)
		return d
	}

	needle := strings.ToLower(d.Query)
	for _, r := range rows {
		// The revision column is a prefix match, so "1" finds 1, 10 and 12.
		if strings.Contains(strings.ToLower(r.Name), needle) ||
			strings.Contains(strings.ToLower(r.RepoURL), needle) ||
			strings.HasPrefix(strconv.FormatInt(r.CurrentRevision, 10), needle) {
			d.Stacks = append(d.Stacks, r)
		}
	}
	d.Count = len(d.Stacks)
	return d
}

// loadStack reads one stack's revisions and last backup for the inspector.
func (c Stacks) loadStack(ctx context.Context, name string) stackDetailData {
	d := stackDetailData{Name: name}

	if _, _, configured := c.loadParams(ctx); !configured {
		d.ErrKey = "err.api_not_configured"
		return d
	}

	det, err := c.API.GetStack(ctx, name)
	if err != nil {
		d.ErrKey, d.ErrRaw = "err.stack", err.Error()
		return d
	}

	sd := &stackDetail{Stack: stackInfo{
		Name:            det.Stack.Name,
		CurrentRevision: det.Stack.CurrentRevision,
		RepoURL:         det.Stack.RepoURL,
		CreatedAt:       det.Stack.CreatedAt,
		UpdatedAt:       det.Stack.UpdatedAt,
	}}
	if sd.Stack.Name == "" {
		sd.Stack.Name = name
	}
	for _, r := range det.Revisions {
		sd.Revisions = append(sd.Revisions, revRow{
			Revision:  r.Revision,
			CreatedAt: r.CreatedAt,
			Current:   r.Revision == det.Stack.CurrentRevision,
		})
	}
	if b := det.LastBackup; b != nil {
		sd.LastBackup = &backupInfo{
			Status:       b.Status,
			Tone:         backupTone(b.Status),
			StartedAt:    b.StartedAt,
			FinishedAt:   b.FinishedAt,
			Revision:     b.Revision,
			ErrorMessage: b.ErrorMessage,
		}
	}
	d.Detail = sd
	return d
}

// backupTone maps the statuses the daemon reports onto the pill tones that
// exist in the stylesheet. The status word itself is rendered verbatim: it is
// the daemon's own vocabulary, not console copy.
func backupTone(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "succeeded", "success", "completed", "done", "ok":
		return "good"
	case "failed", "error":
		return "bad"
	case "running", "started", "pending", "queued", "in_progress":
		return "warn"
	default:
		return "plain"
	}
}

// ShowBackups answers the per-stack backups route. The backups page and its
// fragment belong to the backups worker, and this controller must not build
// another page's model or reach into its helpers: the route hands the operator
// to the backups page, which owns listing and filtering them.
func (c Stacks) ShowBackups(g *gin.Context) {
	g.Redirect(http.StatusFound, WebBase+"/backups")
}
