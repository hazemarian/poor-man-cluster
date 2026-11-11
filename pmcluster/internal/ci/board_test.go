package ci

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
)

// The board's whole value is that it answers "what is happening" without lying and
// without spending the API budget. Both are behavioural, so both are tested here
// against a fake Source — no network, no token.

type fakeSource struct {
	repos     []string
	reposErr  error
	runs      []githubapp.WorkflowRun
	steps     map[int64][2]string
	runCalls  int
	repoCalls int
	stepCalls int
}

func (f *fakeSource) InstallationRepos(context.Context) ([]string, error) {
	f.repoCalls++
	if f.reposErr != nil {
		return nil, f.reposErr
	}
	return f.repos, nil
}

func (f *fakeSource) LatestRuns(_ context.Context, _ []string) []githubapp.WorkflowRun {
	f.runCalls++
	return append([]githubapp.WorkflowRun(nil), f.runs...)
}

func (f *fakeSource) FailedStep(_ context.Context, _ string, id int64) (string, string, error) {
	f.stepCalls++
	if s, ok := f.steps[id]; ok {
		return s[0], s[1], nil
	}
	return "", "", nil
}

func newBoard(src Source, now func() time.Time) *Board {
	return &Board{Src: src, Now: now}
}

func TestSnapshot_CountsAndOrders(t *testing.T) {
	now := time.Unix(1700000000, 0)
	src := &fakeSource{
		repos: []string{"acme/api", "acme/web", "acme/db"},
		runs: []githubapp.WorkflowRun{
			{Repo: "acme/api", Workflow: "CI", Status: "in_progress", UpdatedAt: now.Add(-1 * time.Minute)},
			{Repo: "acme/web", Workflow: "CI", Status: "queued", UpdatedAt: now.Add(-5 * time.Minute)},
			{Repo: "acme/db", Workflow: "CI", Status: "completed", Conclusion: "failure", ID: 5, UpdatedAt: now.Add(-2 * time.Minute)},
		},
		steps: map[int64][2]string{5: {"deploy", "Deploy stack"}},
	}
	b := newBoard(src, func() time.Time { return now })

	snap := b.Snapshot(context.Background())
	if snap.Queued != 1 || snap.Running != 1 || snap.Failed != 1 {
		t.Fatalf("counts = queued %d running %d failed %d, want 1/1/1", snap.Queued, snap.Running, snap.Failed)
	}
	if got := snap.Runs[0].Repo; got != "acme/api" {
		t.Errorf("first row = %q, want the most recently updated (acme/api)", got)
	}
	if snap.Runs[2].Repo != "acme/web" {
		t.Errorf("last row = %q, want the oldest (acme/web)", snap.Runs[2].Repo)
	}
}

// The cache is the design: a second read inside the TTL must not touch GitHub.
func TestSnapshot_SecondReadInsideTheTTLDoesNotCallGitHub(t *testing.T) {
	now := time.Unix(1700000000, 0)
	src := &fakeSource{repos: []string{"acme/api"}, runs: []githubapp.WorkflowRun{{Repo: "acme/api", ID: 1}}}
	b := newBoard(src, func() time.Time { return now })

	b.Snapshot(context.Background())
	b.Snapshot(context.Background())
	b.Snapshot(context.Background())
	if src.runCalls != 1 || src.repoCalls != 1 {
		t.Fatalf("runs=%d repos=%d, want one of each for three reads inside the TTL", src.runCalls, src.repoCalls)
	}

	// Past the run TTL, but inside the repo TTL: re-read the runs, not the repos.
	now = now.Add(RunTTL + time.Second)
	b.Snapshot(context.Background())
	if src.runCalls != 2 {
		t.Errorf("runs read %d times, want 2 after the run TTL expired", src.runCalls)
	}
	if src.repoCalls != 1 {
		t.Errorf("repos read %d times, want 1 — the repo list changes when a repo is created, not when a build finishes", src.repoCalls)
	}

	// Past the repo TTL too.
	now = now.Add(RepoTTL + time.Second)
	b.Snapshot(context.Background())
	if src.repoCalls != 2 {
		t.Errorf("repos read %d times, want 2 after the repo TTL expired", src.repoCalls)
	}
}

// A failing step is fetched once per run, then remembered: it cannot change once
// the run is complete, and this is the expensive call.
func TestSnapshot_FailingStepIsFetchedOncePerRun(t *testing.T) {
	now := time.Unix(1700000000, 0)
	src := &fakeSource{
		repos: []string{"acme/api"},
		runs:  []githubapp.WorkflowRun{{Repo: "acme/api", ID: 42, Conclusion: "failure", UpdatedAt: now}},
		steps: map[int64][2]string{42: {"build", "Run tests"}},
	}
	b := newBoard(src, func() time.Time { return now })

	b.Snapshot(context.Background())
	now = now.Add(RunTTL + time.Second) // force a run refresh
	snap := b.Snapshot(context.Background())

	if src.stepCalls != 1 {
		t.Errorf("FailedStep called %d times, want 1 (cached after the first)", src.stepCalls)
	}
	if snap.Runs[0].FailedStep != "Run tests" {
		t.Errorf("failed step = %q, want it carried from the cache", snap.Runs[0].FailedStep)
	}
}

// The cap exists so a fleet-wide breakage cannot turn one page load into a hundred
// API calls — the reason this feature does not poll.
func TestSnapshot_NamesOnlyTheMostRecentFailures(t *testing.T) {
	now := time.Unix(1700000000, 0)
	var runs []githubapp.WorkflowRun
	for i := int64(1); i <= 9; i++ {
		runs = append(runs, githubapp.WorkflowRun{Repo: "acme/r", ID: i, Conclusion: "failure", UpdatedAt: now.Add(-time.Duration(i) * time.Minute)})
	}
	src := &fakeSource{repos: []string{"acme/r"}, runs: runs}
	b := newBoard(src, func() time.Time { return now })
	b.Snapshot(context.Background())
	if src.stepCalls != maxFailures {
		t.Fatalf("FailedStep called %d times, want the cap of %d", src.stepCalls, maxFailures)
	}
}

// "Why cancelled" is inference. A cancelled run with a newer run of the same
// workflow is marked superseded; one without is left alone so the UI can say
// "cancelled (operator or API)".
func TestSnapshot_InferenceForCancelledRuns(t *testing.T) {
	now := time.Unix(1700000000, 0)
	src := &fakeSource{
		repos: []string{"acme/api", "acme/web"},
		runs: []githubapp.WorkflowRun{
			{Repo: "acme/api", Workflow: "CI", ID: 10, Conclusion: "cancelled", UpdatedAt: now.Add(-2 * time.Minute)},
			{Repo: "acme/web", Workflow: "CI", ID: 20, Conclusion: "cancelled", UpdatedAt: now.Add(-2 * time.Minute)},
			{Repo: "acme/api", Workflow: "CI", ID: 11, Conclusion: "failure", UpdatedAt: now.Add(-1 * time.Minute)},
		},
	}
	b := newBoard(src, func() time.Time { return now })
	snap := b.Snapshot(context.Background())

	byID := map[int64]githubapp.WorkflowRun{}
	for _, r := range snap.Runs {
		byID[r.ID] = r
	}
	if byID[10].SupersededBy != 11 {
		t.Errorf("api run 10 superseded_by = %d, want 11", byID[10].SupersededBy)
	}
	if byID[20].SupersededBy != 0 {
		t.Errorf("web run 20 superseded_by = %d, want 0 (nothing newer)", byID[20].SupersededBy)
	}
}

// A repo list failure keeps the last good board and reports the error, rather than
// blanking the panel at the moment an operator needs it.
func TestSnapshot_RepoListFailureKeepsTheLastBoard(t *testing.T) {
	now := time.Unix(1700000000, 0)
	src := &fakeSource{repos: []string{"acme/api"}, runs: []githubapp.WorkflowRun{{Repo: "acme/api", ID: 1}}}
	b := newBoard(src, func() time.Time { return now })
	b.Snapshot(context.Background())

	src.reposErr = errors.New("github is down")
	now = now.Add(RepoTTL + time.Second)
	snap := b.Snapshot(context.Background())

	if snap.Error == "" {
		t.Error("the failure was not reported")
	}
	if len(snap.Runs) != 1 {
		t.Errorf("runs = %d, want the last good board kept", len(snap.Runs))
	}
}

// A repo GitHub will not let us read must still appear, with the reason.
func TestSnapshot_UnreadableRepoIsKept(t *testing.T) {
	now := time.Unix(1700000000, 0)
	src := &fakeSource{
		repos: []string{"acme/secret"},
		runs:  []githubapp.WorkflowRun{{Repo: "acme/secret", Unreadable: "not readable with this App's permissions", UpdatedAt: now}},
	}
	b := newBoard(src, func() time.Time { return now })
	snap := b.Snapshot(context.Background())
	if len(snap.Runs) != 1 || snap.Runs[0].Unreadable == "" {
		t.Fatalf("snapshot = %+v, want the unreadable repo kept with its reason", snap.Runs)
	}
}
