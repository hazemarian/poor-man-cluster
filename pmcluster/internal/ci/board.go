// Package ci renders the CI board: what every repository's latest workflow run is
// doing, read on demand, cached so that opening the page three times is not three
// trips to the GitHub API.
//
// Two rules run through it:
//
//   - Never claim more than was observed. A repo the App cannot read is a row with
//     a reason, not a missing row: hiding it makes "no information" look like
//     "everything is fine", which is the failure mode this board exists to end.
//   - Say when something is inference. GitHub does not report who cancelled a run,
//     so "superseded" is derived from a newer run existing, and the UI must label
//     it as such rather than presenting a guess as a fact.
package ci

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
)

// Board is the CI view: what every repo's latest run is doing, read on demand.
//
// The caching is the design, not an optimisation. The console is opened in bursts
// — an operator refreshing a page three times while a build runs — and each open
// is one API call per repo. Without a TTL that is ~26 calls per refresh against an
// installation token's budget, for a board nobody is watching between refreshes.
// So: the run list is cached for a minute, the repo list for ten (it changes when
// somebody creates a repo, not when a build finishes), and the failing-step lookup
// — the expensive one, one call per failed repo — for five minutes, because a
// completed run's failing step never changes.
const (
	RunTTL      = 60 * time.Second
	RepoTTL     = 10 * time.Minute
	FailedTTL   = 5 * time.Minute
	maxFailures = 5 // name the step for the most recent failures only
)

// Source is what the board needs from GitHub. An interface so the board's
// behaviour — caching, counts, cancellation inference — is testable without a
// network or a token.
type Source interface {
	InstallationRepos(ctx context.Context) ([]string, error)
	LatestRuns(ctx context.Context, repos []string) []githubapp.WorkflowRun
	FailedStep(ctx context.Context, repo string, runID int64) (job, step string, err error)
}

// Snapshot is one rendering of the board.
type Snapshot struct {
	Runs []githubapp.WorkflowRun `json:"runs"`
	// Counts are over Runs, so the header cannot disagree with the table.
	Queued  int `json:"queued"`
	Running int `json:"running"`
	Failed  int `json:"failed"`
	// FetchedAt is when the underlying data was read, not when it was served:
	// a cached snapshot says how stale it is instead of pretending to be live.
	FetchedAt time.Time `json:"fetched_at"`
	// Cached is true when this answer came from the TTL cache.
	Cached bool `json:"cached"`
	// Error is set when the repo list itself could not be read; Runs is then
	// whatever the last successful read produced.
	Error string `json:"error,omitempty"`
}

// Board caches one snapshot and the two slower-moving pieces behind it.
type Board struct {
	Src Source
	Now func() time.Time

	mu        sync.Mutex
	repos     []string
	reposAt   time.Time
	runs      []githubapp.WorkflowRun
	runsAt    time.Time
	failSteps map[int64]struct{ job, step string }
}

// Snapshot returns the board, refreshing whatever has expired.
func (b *Board) Snapshot(ctx context.Context) Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Now == nil {
		b.Now = time.Now
	}
	now := b.Now()

	if b.repos == nil || now.Sub(b.reposAt) > RepoTTL {
		repos, err := b.Src.InstallationRepos(ctx)
		if err != nil {
			// Keep the last good list: a transient API failure should not empty
			// the board. The error rides along so the panel can say so.
			snap := b.snapshotLocked(now, false)
			snap.Error = err.Error()
			return snap
		}
		b.repos, b.reposAt = repos, now
	}

	if b.runs == nil || now.Sub(b.runsAt) > RunTTL {
		b.runs = b.Src.LatestRuns(ctx, b.repos)
		b.runsAt = now
		b.failSteps = b.nameFailures(ctx)
		b.inferCancellations()
	}

	return b.snapshotLocked(now, now.Sub(b.runsAt) > 0 && now.Sub(b.runsAt) < RunTTL && !b.runsAt.Equal(now))
}

func (b *Board) snapshotLocked(now time.Time, cached bool) Snapshot {
	out := Snapshot{
		Runs:      append([]githubapp.WorkflowRun(nil), b.runs...),
		FetchedAt: b.runsAt,
		Cached:    cached,
	}
	for _, r := range out.Runs {
		switch strings.ToLower(r.Status) {
		case "queued", "pending", "waiting":
			out.Queued++
		case "in_progress":
			out.Running++
		}
		if strings.EqualFold(r.Conclusion, "failure") {
			out.Failed++
		}
	}
	// Newest activity first: the question "what is happening" is answered from
	// the top, and alphabetising by repo buries the thing that just changed.
	sort.SliceStable(out.Runs, func(i, j int) bool {
		return out.Runs[i].UpdatedAt.After(out.Runs[j].UpdatedAt)
	})
	return out
}

// nameFailures fills the failing job/step for the most recent failures, capped so
// a fleet-wide breakage cannot turn one page load into a hundred API calls.
func (b *Board) nameFailures(ctx context.Context) map[int64]struct{ job, step string } {
	if b.failSteps == nil {
		b.failSteps = map[int64]struct{ job, step string }{}
	}
	named := 0
	for i := range b.runs {
		r := &b.runs[i]
		if !strings.EqualFold(r.Conclusion, "failure") || r.ID == 0 {
			continue
		}
		if got, ok := b.failSteps[r.ID]; ok {
			r.FailedJob, r.FailedStep = got.job, got.step
			continue
		}
		if named >= maxFailures {
			continue
		}
		// Count the ATTEMPT, not the success: the cap exists to bound API calls,
		// and a repo whose lookup fails or returns nothing still cost one.
		named++
		job, step, err := b.Src.FailedStep(ctx, r.Repo, r.ID)
		if err != nil || job == "" {
			continue
		}
		b.failSteps[r.ID] = struct{ job, step string }{job, step}
		r.FailedJob, r.FailedStep = job, step
	}
	return b.failSteps
}

// inferCancellations marks a cancelled run as superseded when a NEWER run of the
// same workflow exists.
//
// This is inference and the UI must label it as such: GitHub's API does not say
// who cancelled a run, and today all three causes — a newer commit, an operator,
// a competing dispatcher — arrive as the same word "cancelled".
func (b *Board) inferCancellations() {
	byWorkflow := map[string]int64{}
	for _, r := range b.runs {
		key := r.Repo + "\x00" + r.Workflow
		if r.ID > byWorkflow[key] {
			byWorkflow[key] = r.ID
		}
	}
	for i := range b.runs {
		r := &b.runs[i]
		if !strings.EqualFold(r.Conclusion, "cancelled") || r.ID == 0 {
			continue
		}
		if newest := byWorkflow[r.Repo+"\x00"+r.Workflow]; newest > r.ID {
			r.SupersededBy = newest
		}
	}
}
