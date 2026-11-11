package githubapp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The CI board is read on demand through the installation token, so these tests
// pin the wire shape and — more importantly — the two decisions that keep a board
// honest: an unreadable repo becomes a ROW with a reason (never a silently missing
// row), and a repo with no runs is a row with no state (never an error).

func ciTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	return runnerTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("call to %s carried no bearer token", r.URL.Path)
		}
		h(w, r)
	})
}

func TestLatestRuns_ShapesTheRow(t *testing.T) {
	c := ciTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/api/actions/runs" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("per_page"); got != "1" {
			t.Errorf("per_page = %q, want 1: the board needs the latest run only", got)
		}
		_, _ = io.WriteString(w, `{"workflow_runs":[{
			"id": 99, "name": "CI", "event": "push", "status": "completed",
			"conclusion": "failure", "head_sha": "abc1234def",
			"head_branch": "main", "html_url": "https://github.com/acme/api/actions/runs/99",
			"updated_at": "2026-11-11T04:30:00Z"}]}`)
	})

	runs := c.LatestRuns(context.Background(), "tok", []string{"acme/api"})
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	got := runs[0]
	if got.ID != 99 || got.Workflow != "CI" || got.Conclusion != "failure" || got.Branch != "main" {
		t.Fatalf("row = %+v", got)
	}
	if got.HeadSHA != "abc1234def" {
		t.Errorf("head sha = %q, want it carried through (the row shortens it)", got.HeadSHA)
	}
	if got.Unreadable != "" {
		t.Errorf("a readable repo was marked unreadable: %q", got.Unreadable)
	}
}

func TestLatestRuns_UnreadableRepoIsARowNotAGap(t *testing.T) {
	c := ciTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
	})

	runs := c.LatestRuns(context.Background(), "tok", []string{"acme/secret"})
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1 — hiding the row makes \"no information\" look like \"fine\"", len(runs))
	}
	if runs[0].Repo != "acme/secret" || runs[0].Unreadable == "" {
		t.Fatalf("row = %+v, want it kept with a reason", runs[0])
	}
	if !strings.Contains(runs[0].Unreadable, "not readable") {
		t.Errorf("reason = %q, want it to say the App cannot read this repo", runs[0].Unreadable)
	}
}

func TestLatestRuns_RepoWithNoWorkflowRunsIsARow(t *testing.T) {
	c := ciTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"workflow_runs":[]}`)
	})
	runs := c.LatestRuns(context.Background(), "tok", []string{"acme/fresh"})
	if len(runs) != 1 || runs[0].Repo != "acme/fresh" {
		t.Fatalf("runs = %+v, want one row for a repo that has never run", runs)
	}
	if runs[0].Unreadable != "" || runs[0].ID != 0 {
		t.Errorf("row = %+v, want no state and no error", runs[0])
	}
}

func TestFailedStep_NamesTheStepThatFailed(t *testing.T) {
	c := ciTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/api/actions/runs/99/jobs" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"jobs":[
			{"name":"build","conclusion":"success","steps":[{"name":"checkout","conclusion":"success"}]},
			{"name":"deploy","conclusion":"failure","steps":[
				{"name":"checkout","conclusion":"success"},
				{"name":"Deploy stack","conclusion":"failure"},
				{"name":"Notify","conclusion":"skipped"}]}]}`)
	})
	job, step, err := c.FailedStep(context.Background(), "tok", "acme/api", 99)
	if err != nil {
		t.Fatalf("FailedStep: %v", err)
	}
	if job != "deploy" || step != "Deploy stack" {
		t.Fatalf("job=%q step=%q, want deploy/Deploy stack", job, step)
	}
}

// A job can fail with no failing step (the runner never started it). The row must
// still name the job rather than showing a blank cell.
func TestFailedStep_JobFailureWithNoFailingStep(t *testing.T) {
	c := ciTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"jobs":[{"name":"build","conclusion":"failure","steps":[]}]}`)
	})
	job, step, err := c.FailedStep(context.Background(), "tok", "acme/api", 7)
	if err != nil {
		t.Fatalf("FailedStep: %v", err)
	}
	if job != "build" || step == "" {
		t.Fatalf("job=%q step=%q, want the job named and the step explained", job, step)
	}
}

func TestInstallationRepos_SkipsArchived(t *testing.T) {
	c := ciTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/installation/repositories" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"repositories":[
			{"full_name":"acme/api"},{"full_name":"acme/old","archived":true},{"full_name":"acme/web"}]}`)
	})
	repos, err := c.InstallationRepos(context.Background(), "tok")
	if err != nil {
		t.Fatalf("InstallationRepos: %v", err)
	}
	if len(repos) != 2 || repos[0] != "acme/api" || repos[1] != "acme/web" {
		t.Fatalf("repos = %v, want the two live ones (archived repos do not build)", repos)
	}
}
