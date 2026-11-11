package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ActionsRuns is the CI surface: the latest workflow run per repository, and the
// step that failed.
//
// Read on demand with the App's existing `actions: read`, never polled: the
// console asks when a page is opened, and the daemon caches the answer. A
// background poller would spend the installation's rate limit on a board nobody
// is looking at.
//
// This is also the only way to answer "why is it red" without sending an operator
// to github.com — the runner API knows a runner is busy, not what it is building.

// ErrActionsNotReadable means the installation cannot see this repo's runs. It is
// data for the row ("not readable"), not a failure of the page: hiding the row
// would make absence of information look like everything is fine.
var ErrActionsNotReadable = errors.New("actions not readable")

// WorkflowRun is one run, flattened to what a board needs.
type WorkflowRun struct {
	ID         int64     `json:"id"`
	Repo       string    `json:"repo"`
	Workflow   string    `json:"workflow"`
	Event      string    `json:"event"`
	Status     string    `json:"status"`     // queued, in_progress, completed
	Conclusion string    `json:"conclusion"` // success, failure, cancelled, …
	HeadSHA    string    `json:"head_sha"`
	Branch     string    `json:"branch"`
	URL        string    `json:"html_url"`
	UpdatedAt  time.Time `json:"updated_at"`
	// FailedJob and FailedStep are filled only for a failed run, with a second
	// call that is cached longer than the list itself.
	FailedJob  string `json:"failed_job,omitempty"`
	FailedStep string `json:"failed_step,omitempty"`
	// SupersededBy is the id of a newer run of the same workflow+branch, when the
	// run was cancelled and one exists. GitHub does not say who cancelled a run,
	// so anything derived from this is INFERENCE and the UI must say so.
	SupersededBy int64 `json:"superseded_by,omitempty"`
	// Unreadable names why this repo could not be read, when it could not.
	Unreadable string `json:"unreadable,omitempty"`
}

type runsJSON struct {
	WorkflowRuns []struct {
		ID         int64     `json:"id"`
		Name       string    `json:"name"`
		Event      string    `json:"event"`
		Status     string    `json:"status"`
		Conclusion string    `json:"conclusion"`
		HeadSHA    string    `json:"head_sha"`
		HeadBranch string    `json:"head_branch"`
		HTMLURL    string    `json:"html_url"`
		UpdatedAt  time.Time `json:"updated_at"`
	} `json:"workflow_runs"`
}

type jobsJSON struct {
	Jobs []struct {
		Name       string `json:"name"`
		Conclusion string `json:"conclusion"`
		Steps      []struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
		} `json:"steps"`
	} `json:"jobs"`
}

// InstallationRepos lists the repositories the installation can see
// (GET /installation/repositories). This is the board's repo list: it is GitHub's
// own answer to "which repos does this platform build", so nothing is hardcoded
// and a new repo appears without a settings change.
func (c *Client) InstallationRepos(ctx context.Context, bearer string) ([]string, error) {
	body, status, err := c.doToken(ctx, http.MethodGet, "/installation/repositories?per_page=100", bearer, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("list installation repositories: %s", httpErrorText(status, body))
	}
	var resp struct {
		Repositories []struct {
			FullName string `json:"full_name"`
			Archived bool   `json:"archived"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode installation repositories: %w", err)
	}
	out := make([]string, 0, len(resp.Repositories))
	for _, r := range resp.Repositories {
		if r.Archived || r.FullName == "" {
			continue
		}
		out = append(out, r.FullName)
	}
	return out, nil
}

// LatestRuns returns the newest run of each repository, in the order given.
//
// One call per repo (per_page=1). A repo GitHub will not let us read is returned
// with Unreadable set rather than dropped.
func (c *Client) LatestRuns(ctx context.Context, bearer string, repos []string) []WorkflowRun {
	out := make([]WorkflowRun, 0, len(repos))
	for _, repo := range repos {
		run, err := c.latestRun(ctx, bearer, repo)
		if err != nil {
			out = append(out, WorkflowRun{Repo: repo, Unreadable: unreadableReason(err)})
			continue
		}
		if run == nil {
			// No workflow has ever run: a row with no state, not an error.
			out = append(out, WorkflowRun{Repo: repo})
			continue
		}
		out = append(out, *run)
	}
	return out
}

func (c *Client) latestRun(ctx context.Context, bearer, repo string) (*WorkflowRun, error) {
	path := "/repos/" + repo + "/actions/runs?per_page=1"
	body, status, err := c.doToken(ctx, http.MethodGet, path, bearer, nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusForbidden:
		return nil, fmt.Errorf("%w: %s", ErrActionsNotReadable, httpErrorText(status, body))
	default:
		return nil, fmt.Errorf("latest run for %s: %s", repo, httpErrorText(status, body))
	}
	var resp runsJSON
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode runs for %s: %w", repo, err)
	}
	if len(resp.WorkflowRuns) == 0 {
		return nil, nil
	}
	r := resp.WorkflowRuns[0]
	return &WorkflowRun{
		ID: r.ID, Repo: repo, Workflow: r.Name, Event: r.Event,
		Status: r.Status, Conclusion: r.Conclusion, HeadSHA: r.HeadSHA,
		Branch: r.HeadBranch, URL: r.HTMLURL, UpdatedAt: r.UpdatedAt,
	}, nil
}

// FailedStep names the job and step that failed in a run
// (GET /repos/{owner}/{repo}/actions/runs/{id}/jobs). Only called for failures,
// and cached by the caller for longer than the list, because it is the expensive
// one and its answer never changes once the run is complete.
func (c *Client) FailedStep(ctx context.Context, bearer, repo string, runID int64) (job, step string, err error) {
	if runID <= 0 {
		return "", "", nil
	}
	path := "/repos/" + repo + "/actions/runs/" + strconv.FormatInt(runID, 10) + "/jobs?per_page=50"
	body, status, err := c.doToken(ctx, http.MethodGet, path, bearer, nil)
	if err != nil {
		return "", "", err
	}
	if status != http.StatusOK {
		return "", "", fmt.Errorf("%w: %s", ErrActionsNotReadable, httpErrorText(status, body))
	}
	var resp jobsJSON
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", fmt.Errorf("decode jobs for %s#%d: %w", repo, runID, err)
	}
	for _, j := range resp.Jobs {
		if !strings.EqualFold(j.Conclusion, "failure") {
			continue
		}
		for _, s := range j.Steps {
			if strings.EqualFold(s.Conclusion, "failure") {
				return j.Name, s.Name, nil
			}
		}
		// A job can fail without a step reporting the failure (a runner-level
		// error, e.g. the job never started): name the job and say so.
		return j.Name, "(the job failed before a step did)", nil
	}
	return "", "", nil
}

// unreadableReason turns a client error into the row's text. Anything that is not
// a permission/visibility refusal is still shown, because "the API errored" is
// also something an operator needs to know.
func unreadableReason(err error) string {
	if errors.Is(err, ErrActionsNotReadable) {
		return "not readable with this App's permissions"
	}
	return err.Error()
}
