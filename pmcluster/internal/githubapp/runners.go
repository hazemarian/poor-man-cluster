package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Runner registration is the second consumer of the App's installation token,
// after image pulls and the CI broker. It exists because GitHub-hosted Actions
// minutes run out: a repo-scoped self-hosted runner keeps CI working, and it is
// registered with the SAME hour-lived installation token — no PAT, no long-lived
// credential in a workflow.
//
// The permission this needs is Administration: Read & write. It is deliberately
// NOT part of RequiredPermissionSet: a cluster that only pulls private images
// must not be told it is missing a write permission it never asked for. The
// runner settings/doctor check report it when runners are on.

// PermAdministrationWrite is the permission runner registration requires.
const PermAdministrationWrite = "administration:write"

// RunnerPermissionSet is the permission set runners need, in the shape
// Permissions.Missing takes.
var RunnerPermissionSet = map[string]string{"administration": "write"}

// Runner is one registered runner as GitHub reports it. Status is "online" or
// "offline"; Busy is true while a job is running.
type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

// runnerListJSON is GitHub's wire shape for GET .../actions/runners.
type runnerListJSON struct {
	TotalCount int `json:"total_count"`
	Runners    []struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Busy   bool   `json:"busy"`
	} `json:"runners"`
}

// RunnerRegistrationToken mints a repo-scoped registration token with a
// bearer installation token (POST /repos/{owner}/{repo}/actions/runners/
// registration-token). The token is single-use and valid for one hour: it is
// handed to a runner container at creation time and never persisted.
func (c *Client) RunnerRegistrationToken(ctx context.Context, bearer, repo string) (string, error) {
	if err := validateRepo(repo); err != nil {
		return "", err
	}
	body, status, err := c.doToken(ctx, http.MethodPost, "/repos/"+repo+"/actions/runners/registration-token", bearer, nil)
	if err != nil {
		return "", err
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
	case http.StatusUnauthorized:
		return "", fmt.Errorf("%w: the installation token was refused while minting a runner registration token for %s", ErrTokenRejected, repo)
	case http.StatusForbidden:
		return "", fmt.Errorf("%w: no administration:write on %s — grant the App Repository → Administration: Read & write, then approve it on the installation", ErrInsufficientPermission, repo)
	case http.StatusNotFound:
		return "", fmt.Errorf("%w: %s is not visible to the installation (repository selection is not 'all', or the name is wrong)", ErrInstallationNotFound, repo)
	default:
		return "", fmt.Errorf("mint runner registration token for %s: %s", repo, httpErrorText(status, body))
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("decode runner registration token: %w", err)
	}
	if resp.Token == "" {
		return "", fmt.Errorf("mint runner registration token for %s: GitHub returned an empty token", repo)
	}
	return resp.Token, nil
}

// Runners lists a repo's registered runners
// (GET /repos/{owner}/{repo}/actions/runners?per_page=100).
func (c *Client) Runners(ctx context.Context, bearer, repo string) ([]Runner, error) {
	if err := validateRepo(repo); err != nil {
		return nil, err
	}
	path := "/repos/" + repo + "/actions/runners?per_page=100"
	body, status, err := c.doToken(ctx, http.MethodGet, path, bearer, nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: the installation token was refused while listing runners for %s", ErrTokenRejected, repo)
	case http.StatusForbidden:
		return nil, fmt.Errorf("%w: no administration:write on %s — grant the App Repository → Administration: Read & write and approve it on the installation", ErrInsufficientPermission, repo)
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s is not visible to the installation", ErrInstallationNotFound, repo)
	default:
		return nil, fmt.Errorf("list runners for %s: %s", repo, httpErrorText(status, body))
	}
	var resp runnerListJSON
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode runners for %s: %w", repo, err)
	}
	out := make([]Runner, 0, len(resp.Runners))
	for _, r := range resp.Runners {
		out = append(out, Runner{ID: r.ID, Name: r.Name, Status: r.Status, Busy: r.Busy})
	}
	return out, nil
}

// DeleteRunner de-registers one runner
// (DELETE /repos/{owner}/{repo}/actions/runners/{id}). A runner GitHub no
// longer knows (404) is success: the delete is idempotent, which is what a
// reconcile retrying after a partial failure needs.
func (c *Client) DeleteRunner(ctx context.Context, bearer, repo string, id int64) error {
	if err := validateRepo(repo); err != nil {
		return err
	}
	if id <= 0 {
		return fmt.Errorf("delete runner: id must be positive, got %d", id)
	}
	path := fmt.Sprintf("/repos/%s/actions/runners/%d", repo, id)
	body, status, err := c.doToken(ctx, http.MethodDelete, path, bearer, nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: the installation token was refused while deleting runner %d from %s", ErrTokenRejected, id, repo)
	case http.StatusForbidden:
		return fmt.Errorf("%w: no administration:write on %s, so runner %d cannot be de-registered", ErrInsufficientPermission, repo, id)
	default:
		return fmt.Errorf("delete runner %d from %s: %s", id, repo, httpErrorText(status, body))
	}
}

// RepoPrivate reports whether repo is private (GET /repos/{owner}/{repo}).
// The platform refuses to register a runner for a public repository, because a
// fork's pull request would run arbitrary workflow code on a container with the
// host's docker socket mounted.
func (c *Client) RepoPrivate(ctx context.Context, bearer, repo string) (bool, error) {
	if err := validateRepo(repo); err != nil {
		return false, err
	}
	body, status, err := c.doToken(ctx, http.MethodGet, "/repos/"+repo, bearer, nil)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return false, fmt.Errorf("%w: the installation token was refused while reading %s", ErrTokenRejected, repo)
	case http.StatusForbidden:
		return false, fmt.Errorf("%w: %s is not readable by the installation", ErrInsufficientPermission, repo)
	case http.StatusNotFound:
		return false, fmt.Errorf("%w: %s is not visible to the installation", ErrInstallationNotFound, repo)
	default:
		return false, fmt.Errorf("read repository %s: %s", repo, httpErrorText(status, body))
	}
	var resp struct {
		Private bool `json:"private"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return false, fmt.Errorf("decode repository %s: %w", repo, err)
	}
	return resp.Private, nil
}

// ---- Local: the control-plane adapter --------------------------------------

// RunnerRegistrationToken implements runners.GitHub with the cached
// installation token.
func (l *Local) RunnerRegistrationToken(ctx context.Context, repo string) (string, error) {
	tok, err := l.Token(ctx)
	if err != nil {
		return "", err
	}
	return l.Client.RunnerRegistrationToken(ctx, tok.Token, repo)
}

// ListRunners implements runners.GitHub.
func (l *Local) ListRunners(ctx context.Context, repo string) ([]Runner, error) {
	tok, err := l.Token(ctx)
	if err != nil {
		return nil, err
	}
	return l.Client.Runners(ctx, tok.Token, repo)
}

// DeleteRunner implements runners.GitHub.
func (l *Local) DeleteRunner(ctx context.Context, repo string, id int64) error {
	tok, err := l.Token(ctx)
	if err != nil {
		return err
	}
	return l.Client.DeleteRunner(ctx, tok.Token, repo, id)
}

// RepoPrivate implements runners.GitHub.
func (l *Local) RepoPrivate(ctx context.Context, repo string) (bool, error) {
	tok, err := l.Token(ctx)
	if err != nil {
		return false, err
	}
	return l.Client.RepoPrivate(ctx, tok.Token, repo)
}

// GrantedPermissions implements runners.GitHub: it reads the App's own
// granted set (GET /app), which is what the doctor reports a missing
// administration:write from.
func (l *Local) GrantedPermissions(ctx context.Context) (map[string]string, error) {
	row, err := l.Store.GitHubApp(ctx)
	if err != nil {
		return nil, err
	}
	plain, err := l.decryptKey(row)
	if err != nil {
		return nil, err
	}
	app, err := NewApp(row.AppID, plain)
	if err != nil {
		return nil, err
	}
	perms, err := l.Client.Permissions(ctx, app)
	if err != nil {
		return nil, err
	}
	return perms.Permissions, nil
}
