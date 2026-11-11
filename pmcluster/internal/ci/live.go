package ci

import (
	"context"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
)

// Live adapts the GitHub App to the board's Source.
//
// The App already holds `actions: read` for the runner work, so the board needs no
// new permission, no webhook subscription and no background poller: it asks when a
// page is opened and the Board decides how often that becomes a real API call.
type Live struct {
	App    *githubapp.Local
	Client *githubapp.Client
}

// InstallationRepos is GitHub's own answer to "which repos does this platform
// build", so a new repo appears on the board without a settings change.
func (l *Live) InstallationRepos(ctx context.Context) ([]string, error) {
	tok, err := l.App.Token(ctx)
	if err != nil {
		return nil, err
	}
	return l.Client.InstallationRepos(ctx, tok.Token)
}

func (l *Live) LatestRuns(ctx context.Context, repos []string) []githubapp.WorkflowRun {
	tok, err := l.App.Token(ctx)
	if err != nil {
		// Every repo is unreadable for the same reason: say it once per row
		// rather than returning an empty board that looks healthy.
		out := make([]githubapp.WorkflowRun, 0, len(repos))
		for _, r := range repos {
			out = append(out, githubapp.WorkflowRun{Repo: r, Unreadable: "no installation token: " + err.Error()})
		}
		return out
	}
	return l.Client.LatestRuns(ctx, tok.Token, repos)
}

func (l *Live) FailedStep(ctx context.Context, repo string, runID int64) (string, string, error) {
	tok, err := l.App.Token(ctx)
	if err != nil {
		return "", "", err
	}
	return l.Client.FailedStep(ctx, tok.Token, repo, runID)
}
