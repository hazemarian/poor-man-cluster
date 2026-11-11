package controllers

import (
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// CI is the CI board: one row per repository, newest activity first, so "what is
// my CI doing" is answerable without opening ten repository pages.
//
// It is a READ of state the daemon already caches. Nothing here acts on a run —
// cancelling, re-running and dispatching belong to git and GitHub, not to a
// platform console that would then need to explain which of three triggers
// cancelled what.
type CI struct{ *Controller }

type ciRow struct {
	Repo     string
	Workflow string
	State    string // one of the ci.state_* keys
	Class    string // pill class: good, warn, bad, plain
	SHA      string
	Age      string
	// Detail is the failing job/step, or why the repo could not be read.
	Detail string
	URL    string
	// Inferred marks a conclusion the API does not actually give us.
	Inferred bool
}

type ciData struct {
	ErrKey string
	ErrRaw string

	Queued  int
	Running int
	Failed  int

	Rows []ciRow
	// FetchedAge is how old the underlying data is, and Cached says it came from
	// the TTL cache: the panel must not imply it is live.
	FetchedAge string
	Cached     bool
	// NotConfigured is set when the daemon has no GitHub App: an empty table would
	// read as "everything is green".
	NotConfigured bool
}

// Page renders the board.
func (c CI) Page(g *gin.Context) {
	d := ciData{}
	snap, err := c.API.CIRuns(g.Request.Context())
	if err != nil {
		// A daemon without the route (an older build) or without an App lands here
		// with a 501: say what is missing rather than showing an empty board.
		if strings.Contains(err.Error(), "not implemented") || strings.Contains(err.Error(), "not configured") {
			d.NotConfigured = true
			c.render(g, d)
			return
		}
		d.ErrKey = "ci.err_load"
		d.ErrRaw = err.Error()
		c.render(g, d)
		return
	}

	d.Queued, d.Running, d.Failed = snap.Queued, snap.Running, snap.Failed
	d.Cached = snap.Cached
	if snap.Error != "" {
		d.ErrKey, d.ErrRaw = "ci.err_partial", snap.Error
	}
	if !snap.FetchedAt.IsZero() {
		d.FetchedAge = humanAge(time.Since(snap.FetchedAt))
	}
	if len(snap.Runs) == 0 && d.ErrKey == "" {
		d.NotConfigured = true
	}
	for _, r := range snap.Runs {
		// Project the wire type explicitly: the mapping below is where the display
		// rules live, and a field added to the API without a decision here should
		// not silently appear on the page.
		d.Rows = append(d.Rows, buildCIRow(ciRowInput{
			ID: r.ID, Repo: r.Repo, Workflow: r.Workflow,
			Status: r.Status, Conclusion: r.Conclusion,
			HeadSHA: r.HeadSHA, UpdatedAt: r.UpdatedAt,
			FailedJob: r.FailedJob, FailedStep: r.FailedStep,
			SupersededBy: r.SupersededBy, Unreadable: r.Unreadable, URL: r.URL,
		}))
	}
	c.render(g, d)
}

func (c CI) render(g *gin.Context, d ciData) {
	c.Views.Fragment(g, "ci", d)
}

// buildCIRow turns one run into a display row. The order of the cases is the
// order of precedence: a repo we could not read is not "no runs", and a cancelled
// run that a newer one replaced says so instead of looking like a failure.
func buildCIRow(r ciRowInput) ciRow {
	row := ciRow{
		Repo:     r.Repo,
		Workflow: r.Workflow,
		SHA:      shortSHA(r.HeadSHA),
		Age:      humanAge(time.Since(r.UpdatedAt)),
		URL:      r.URL,
	}
	switch {
	case r.Unreadable != "":
		row.State, row.Class, row.Detail = "unreadable", "bad", r.Unreadable
	case r.ID == 0:
		row.State, row.Class = "none", "plain"
	case strings.EqualFold(r.Conclusion, "failure"):
		row.State, row.Class = "failed", "bad"
		row.Detail = failingDetail(r.FailedJob, r.FailedStep)
	case strings.EqualFold(r.Status, "in_progress"):
		row.State, row.Class = "running", "warn"
	case isQueued(r.Status):
		row.State, row.Class = "queued", "warn"
	case strings.EqualFold(r.Conclusion, "cancelled"):
		row.State, row.Class = "cancelled", "plain"
		if r.SupersededBy != 0 {
			row.State, row.Detail, row.Inferred = "superseded", "", true
		} else {
			row.Detail, row.Inferred = "", true
		}
	case strings.EqualFold(r.Conclusion, "success"):
		row.State, row.Class = "ok", "good"
	default:
		row.State, row.Class = "other", "plain"
	}
	return row
}

// ciRowInput is the subset of the API row this mapping needs, so it can be tested
// without an HTTP client.
type ciRowInput struct {
	ID           int64
	Repo         string
	Workflow     string
	Status       string
	Conclusion   string
	HeadSHA      string
	UpdatedAt    time.Time
	FailedJob    string
	FailedStep   string
	SupersededBy int64
	Unreadable   string
	URL          string
}

func failingDetail(job, step string) string {
	if job == "" && step == "" {
		return ""
	}
	if step == "" {
		return job
	}
	return job + " → " + step
}

func isQueued(status string) bool {
	switch strings.ToLower(status) {
	case "queued", "pending", "waiting", "requested":
		return true
	}
	return false
}

func shortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

// humanAge is deliberately coarse: a board says "4m", not "4 minutes 12 seconds".
func humanAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
