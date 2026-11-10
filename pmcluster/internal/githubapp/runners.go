package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
)

// Runner registration is the second consumer of the App's installation token,
// after image pulls and the CI broker. It exists because GitHub-hosted Actions
// minutes run out: a self-hosted runner keeps CI working, and it is registered
// with the SAME hour-lived installation token — no PAT, no long-lived credential
// in a workflow.
//
// Registration is ORG level (POST /orgs/{org}/actions/runners/registration-token)
// into a runner GROUP, because one org runner serves every repository the group
// selects. The group's visibility is the only scope GitHub offers at this level,
// which makes it the security boundary: `selected` (the platform creates and
// enforces it) or `all` (every repository in the org, refused — see
// ErrRunnerGroupVisibility).
//
// The permission this needs is Administration: Read & write at the ORGANISATION
// level. It is deliberately NOT part of RequiredPermissionSet: a cluster that
// only pulls private images must not be told it is missing a write permission it
// never asked for. The runner settings/doctor check report it when runners are
// on.
const (
	// PermAdministrationWrite is the permission runner registration requires.
	PermAdministrationWrite = "administration:write"
	// PermActionsRead is what reading the org's newest workflow run needs. It
	// is optional: without it the console's "last job" column stays empty.
	PermActionsRead = "actions:read"
)

// RunnerPermissionSet is the permission set runners need, in the shape
// Permissions.Missing takes.
var RunnerPermissionSet = map[string]string{"administration": "write"}

// GroupVisibilityAll is GitHub's "every repository in the organisation"
// visibility for a runner group.
const (
	GroupVisibilityAll      = "all"
	GroupVisibilitySelected = "selected"
	GroupVisibilityPrivate  = "private"
)

// ErrRunnerGroupVisibility means the group GitHub already had is not restricted
// to selected repositories. It is a refusal, not a retryable failure: an
// all-visible group lets workflow code from ANY repository in the organisation
// run on a container holding the host's docker socket.
var ErrRunnerGroupVisibility = errors.New("runner group visibility is not 'selected'")

// Runner is one registered runner as GitHub reports it. Status is "online" or
// "offline"; Busy is true while a job is running.
type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

// RunnerGroup is one org runner group. Only the fields the platform decides on
// are kept: name, id and the visibility that bounds which repositories may use
// the runner.
type RunnerGroup struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
	Default    bool   `json:"default"`
}

var (
	reOrgLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	reGroup    = regexp.MustCompile(`^[A-Za-z0-9._ -]{1,64}$`)
)

// validateOrg refuses anything that is not a single GitHub org login. The value
// is spliced into API paths, so a path-like value must fail here rather than be
// escaped into a different endpoint.
func validateOrg(org string) error {
	if !reOrgLogin.MatchString(org) {
		return fmt.Errorf("organisation must be a single GitHub org login, got %q", org)
	}
	return nil
}

// validateGroup refuses a group name GitHub would reject, before the call.
func validateGroup(group string) error {
	if !reGroup.MatchString(group) {
		return fmt.Errorf("runner group %q is not a valid GitHub group name", group)
	}
	return nil
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

// OrgRegistrationToken mints an org-scoped registration token with a bearer
// installation token (POST /orgs/{org}/actions/runners/registration-token). The
// token is single-use and valid for one hour: it is handed to a runner container
// at creation time and never persisted.
func (c *Client) OrgRegistrationToken(ctx context.Context, bearer, org string) (string, error) {
	if err := validateOrg(org); err != nil {
		return "", err
	}
	body, status, err := c.doToken(ctx, http.MethodPost, "/orgs/"+org+"/actions/runners/registration-token", bearer, nil)
	if err != nil {
		return "", err
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
	case http.StatusUnauthorized:
		return "", fmt.Errorf("%w: the installation token was refused while minting a runner registration token for org %s", ErrTokenRejected, org)
	case http.StatusForbidden:
		return "", fmt.Errorf("%w: no organization_self_hosted_runners:write for org %s — grant the App Organization → Self-hosted runners: Read & write, then approve it on the installation", ErrInsufficientPermission, org)
	case http.StatusNotFound:
		return "", fmt.Errorf("%w: org %s is not visible to the installation", ErrInstallationNotFound, org)
	default:
		return "", fmt.Errorf("mint runner registration token for org %s: %s", org, httpErrorText(status, body))
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("decode runner registration token: %w", err)
	}
	if resp.Token == "" {
		return "", fmt.Errorf("mint runner registration token for org %s: GitHub returned an empty token", org)
	}
	return resp.Token, nil
}

// OrgRunners lists an organisation's registered runners
// (GET /orgs/{org}/actions/runners?per_page=100).
func (c *Client) OrgRunners(ctx context.Context, bearer, org string) ([]Runner, error) {
	if err := validateOrg(org); err != nil {
		return nil, err
	}
	body, status, err := c.doToken(ctx, http.MethodGet, "/orgs/"+org+"/actions/runners?per_page=100", bearer, nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: the installation token was refused while listing runners for org %s", ErrTokenRejected, org)
	case http.StatusForbidden:
		return nil, fmt.Errorf("%w: no organization_self_hosted_runners:write for org %s — grant the App Organization → Self-hosted runners: Read & write and approve it on the installation", ErrInsufficientPermission, org)
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: org %s is not visible to the installation", ErrInstallationNotFound, org)
	default:
		return nil, fmt.Errorf("list runners for org %s: %s", org, httpErrorText(status, body))
	}
	var resp runnerListJSON
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode runners for org %s: %w", org, err)
	}
	out := make([]Runner, 0, len(resp.Runners))
	for _, r := range resp.Runners {
		out = append(out, Runner{ID: r.ID, Name: r.Name, Status: r.Status, Busy: r.Busy})
	}
	return out, nil
}

// DeleteOrgRunner de-registers one org runner by id
// (DELETE /orgs/{org}/actions/runners/{id}). A runner GitHub no longer knows
// (404) is success: the delete is idempotent, which is what a teardown
// retrying after a partial failure needs.
func (c *Client) DeleteOrgRunner(ctx context.Context, bearer, org string, id int64) error {
	if err := validateOrg(org); err != nil {
		return err
	}
	if id <= 0 {
		return fmt.Errorf("delete runner: id must be positive, got %d", id)
	}
	path := fmt.Sprintf("/orgs/%s/actions/runners/%d", org, id)
	body, status, err := c.doToken(ctx, http.MethodDelete, path, bearer, nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: the installation token was refused while deleting runner %d from org %s", ErrTokenRejected, id, org)
	case http.StatusForbidden:
		return fmt.Errorf("%w: no organization_self_hosted_runners:write for org %s, so runner %d cannot be de-registered", ErrInsufficientPermission, org, id)
	default:
		return fmt.Errorf("delete runner %d from org %s: %s", id, org, httpErrorText(status, body))
	}
}

// RunnerGroups lists an organisation's runner groups
// (GET /orgs/{org}/actions/runner-groups?per_page=100).
func (c *Client) RunnerGroups(ctx context.Context, bearer, org string) ([]RunnerGroup, error) {
	if err := validateOrg(org); err != nil {
		return nil, err
	}
	body, status, err := c.doToken(ctx, http.MethodGet, "/orgs/"+org+"/actions/runner-groups?per_page=100", bearer, nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: the installation token was refused while listing runner groups for org %s", ErrTokenRejected, org)
	case http.StatusForbidden:
		// A 403 here is NOT necessarily a missing permission: GitHub answers
		// 403 for an installation that is not org-wide, for a policy that
		// blocks runner groups, and for a token minted before the permission
		// was approved. Name the likely causes AND carry GitHub's own text,
		// because guessing "no administration:write" sent an operator to
		// check a permission that was already granted.
		return nil, fmt.Errorf("%w: GitHub refused runner groups for org %s (403). The App needs Organization → Self-hosted runners: Read & write (Administration covers repository-level endpoints only) AND the installation must be approved by an org owner; a token minted before the approval keeps the old scopes until it is re-minted. GitHub said: %s",
			ErrInsufficientPermission, org, httpErrorText(http.StatusForbidden, body))
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: org %s is not visible to the installation", ErrInstallationNotFound, org)
	default:
		return nil, fmt.Errorf("list runner groups for org %s: %s", org, httpErrorText(status, body))
	}
	var resp struct {
		Groups []RunnerGroup `json:"runner_groups"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode runner groups for org %s: %w", org, err)
	}
	return resp.Groups, nil
}

// CreateRunnerGroup creates a group with visibility `selected` and the given
// repositories (POST /orgs/{org}/actions/runner-groups).
//
// Visibility is a parameter of this call and never `all`: the platform has no
// code path that creates an all-visible group, because such a runner would take
// jobs from every repository in the org.
func (c *Client) CreateRunnerGroup(ctx context.Context, bearer, org, name string, repoIDs []int64) (RunnerGroup, error) {
	if err := validateOrg(org); err != nil {
		return RunnerGroup{}, err
	}
	if err := validateGroup(name); err != nil {
		return RunnerGroup{}, err
	}
	if repoIDs == nil {
		repoIDs = []int64{}
	}
	payload, err := json.Marshal(map[string]any{
		"name":                    name,
		"visibility":              GroupVisibilitySelected,
		"selected_repository_ids": repoIDs,
	})
	if err != nil {
		return RunnerGroup{}, err
	}
	body, status, err := c.doToken(ctx, http.MethodPost, "/orgs/"+org+"/actions/runner-groups", bearer, payload)
	if err != nil {
		return RunnerGroup{}, err
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
	case http.StatusUnauthorized:
		return RunnerGroup{}, fmt.Errorf("%w: the installation token was refused while creating runner group %q in %s", ErrTokenRejected, name, org)
	case http.StatusForbidden:
		return RunnerGroup{}, fmt.Errorf("%w: no organization_self_hosted_runners:write for org %s, so runner group %q cannot be created", ErrInsufficientPermission, org, name)
	default:
		return RunnerGroup{}, fmt.Errorf("create runner group %q in org %s: %s", name, org, httpErrorText(status, body))
	}
	var g RunnerGroup
	if err := json.Unmarshal(body, &g); err != nil {
		return RunnerGroup{}, fmt.Errorf("decode runner group %q: %w", name, err)
	}
	return g, nil
}

// SetGroupRepositories replaces the selected-repository set of a group
// (PUT /orgs/{org}/actions/runner-groups/{id}/repositories). Called when a
// private repo was added to the org since the group was created, so the group
// keeps covering exactly the private repositories.
func (c *Client) SetGroupRepositories(ctx context.Context, bearer, org string, groupID int64, repoIDs []int64) error {
	if err := validateOrg(org); err != nil {
		return err
	}
	if groupID <= 0 {
		return fmt.Errorf("set group repositories: group id must be positive, got %d", groupID)
	}
	if repoIDs == nil {
		repoIDs = []int64{}
	}
	payload, err := json.Marshal(map[string]any{"selected_repository_ids": repoIDs})
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/orgs/%s/actions/runner-groups/%d/repositories", org, groupID)
	body, status, err := c.doToken(ctx, http.MethodPut, path, bearer, payload)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: the installation token was refused while updating runner group %d in %s", ErrTokenRejected, groupID, org)
	case http.StatusForbidden:
		return fmt.Errorf("%w: no organization_self_hosted_runners:write for org %s, so runner group %d cannot be updated", ErrInsufficientPermission, org, groupID)
	default:
		return fmt.Errorf("update runner group %d in org %s: %s", groupID, org, httpErrorText(status, body))
	}
}

// PrivateRepoIDs lists the ids of an organisation's private repositories
// (GET /orgs/{org}/repos?type=private&per_page=100). These are the members of
// the runner group: a public repository is deliberately never selected, because
// its fork pull requests would run attacker-controlled workflow code on the
// docker socket.
//
// One page of 100 is the documented bound; an org with more private repos than
// that keeps the first 100 selected and the doctor's group warning says so.
func (c *Client) PrivateRepoIDs(ctx context.Context, bearer, org string) ([]int64, error) {
	if err := validateOrg(org); err != nil {
		return nil, err
	}
	body, status, err := c.doToken(ctx, http.MethodGet, "/orgs/"+org+"/repos?type=private&per_page=100", bearer, nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: the installation token was refused while listing %s repositories", ErrTokenRejected, org)
	case http.StatusForbidden:
		return nil, fmt.Errorf("%w: %s repositories are not readable by the installation", ErrInsufficientPermission, org)
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: org %s is not visible to the installation", ErrInstallationNotFound, org)
	default:
		return nil, fmt.Errorf("list %s repositories: %s", org, httpErrorText(status, body))
	}
	var resp []struct {
		ID       int64 `json:"id"`
		Private  bool  `json:"private"`
		Archived bool  `json:"archived"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode %s repositories: %w", org, err)
	}
	out := make([]int64, 0, len(resp))
	for _, r := range resp {
		if !r.Private || r.Archived {
			continue
		}
		out = append(out, r.ID)
	}
	return out, nil
}

// OrgLatestRunURL returns the newest workflow run in the organisation: its
// repository full name and web URL
// (GET /orgs/{org}/actions/runs?per_page=1). It is what the console records as
// a runner's "last job": GitHub's runner API does not attribute a job to the
// runner that ran it, so the honest field is the org's newest run.
//
// This needs Actions: read. A cluster whose App does not grant it gets an error
// here, which the caller logs at debug and treats as "no last job" — never as a
// runner failure.
func (c *Client) OrgLatestRunURL(ctx context.Context, bearer, org string) (string, string, error) {
	if err := validateOrg(org); err != nil {
		return "", "", err
	}
	body, status, err := c.doToken(ctx, http.MethodGet, "/orgs/"+org+"/actions/runs?per_page=1", bearer, nil)
	if err != nil {
		return "", "", err
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return "", "", fmt.Errorf("%w: the installation token was refused while listing runs for org %s", ErrTokenRejected, org)
	case http.StatusNotFound:
		return "", "", fmt.Errorf("%w: org %s is not visible to the installation", ErrInstallationNotFound, org)
	default:
		return "", "", fmt.Errorf("list runs for org %s: %s", org, httpErrorText(status, body))
	}
	var resp struct {
		WorkflowRuns []struct {
			HTMLURL    string `json:"html_url"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", fmt.Errorf("decode runs for org %s: %w", org, err)
	}
	if len(resp.WorkflowRuns) == 0 {
		return "", "", nil
	}
	return resp.WorkflowRuns[0].Repository.FullName, resp.WorkflowRuns[0].HTMLURL, nil
}

// ---- Local: the control-plane adapter --------------------------------------

// RunnerRegistrationToken implements runners.GitHub with the cached
// installation token. The parameter is the organisation.
func (l *Local) RunnerRegistrationToken(ctx context.Context, org string) (string, error) {
	tok, err := l.Token(ctx)
	if err != nil {
		return "", err
	}
	return l.Client.OrgRegistrationToken(ctx, tok.Token, org)
}

// ListRunners implements runners.GitHub.
func (l *Local) ListRunners(ctx context.Context, org string) ([]Runner, error) {
	tok, err := l.Token(ctx)
	if err != nil {
		return nil, err
	}
	return l.Client.OrgRunners(ctx, tok.Token, org)
}

// DeleteRunner implements runners.GitHub.
func (l *Local) DeleteRunner(ctx context.Context, org string, id int64) error {
	tok, err := l.Token(ctx)
	if err != nil {
		return err
	}
	return l.Client.DeleteOrgRunner(ctx, tok.Token, org, id)
}

// EnsureRunnerGroup implements runners.GitHub: it creates the group with
// `selected` visibility when it is absent, and refuses an existing group whose
// visibility is anything but `selected` — `all` would let any repository in the
// organisation schedule a job onto a container holding the docker socket.
//
// The selected-repository set is kept in step with the org's private
// repositories: a group with no repositories takes no jobs, and a group
// containing public repositories would expose the runner to fork PRs.
func (l *Local) EnsureRunnerGroup(ctx context.Context, org, group string) (RunnerGroup, error) {
	tok, err := l.Token(ctx)
	if err != nil {
		return RunnerGroup{}, err
	}
	groups, err := l.Client.RunnerGroups(ctx, tok.Token, org)
	if err != nil {
		return RunnerGroup{}, err
	}
	repos, err := l.Client.PrivateRepoIDs(ctx, tok.Token, org)
	if err != nil {
		return RunnerGroup{}, err
	}
	for _, g := range groups {
		if g.Name != group {
			continue
		}
		if g.Visibility != GroupVisibilitySelected {
			return g, fmt.Errorf("%w: runner group %q in org %s has visibility %q", ErrRunnerGroupVisibility, group, org, g.Visibility)
		}
		if err := l.Client.SetGroupRepositories(ctx, tok.Token, org, g.ID, repos); err != nil {
			return g, err
		}
		return g, nil
	}
	return l.Client.CreateRunnerGroup(ctx, tok.Token, org, group, repos)
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

// LatestRunURL implements runners.GitHub with the cached installation token.
func (l *Local) LatestRunURL(ctx context.Context, org string) (string, string, error) {
	tok, err := l.Token(ctx)
	if err != nil {
		return "", "", err
	}
	return l.Client.OrgLatestRunURL(ctx, tok.Token, org)
}
