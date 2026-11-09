package runners

import (
	"context"
	"fmt"
	"strings"
)

// RequiredPermission is the App permission runner registration needs.
// `administration` at `write` is what POST .../actions/runners/registration-
// token requires; read is not enough, and the failure GitHub returns for a
// missing permission is a bare 403 ("Resource not accessible by integration")
// that names neither the permission nor the fix — which is why the doctor
// reports this before a registration is ever attempted.
const RequiredPermission = "administration:write"

// Report is the doctor's view: enabled or not, and per repo whether the
// container and the GitHub registration agree.
type Report struct {
	Enabled bool `json:"enabled"`
	// AppConfigured is false when no GitHub App registration exists, which
	// makes an enabled feature inert (and is reported as a failure: enabling
	// it is refused without an App, so this means the App was cleared later).
	AppConfigured bool `json:"app_configured"`
	// MissingPermission names RequiredPermission when the App's granted set
	// does not include it.
	MissingPermission string       `json:"missing_permission,omitempty"`
	Repos             []RepoReport `json:"repos,omitempty"`
	// Warnings are per-repo failures plus anything Inspect itself could not
	// read (a GitHub call that failed, an unreadable service list).
	Warnings []string `json:"warnings,omitempty"`
}

// RepoReport is one repo's state.
type RepoReport struct {
	Repo             string `json:"repo"`
	RunnerName       string `json:"runner_name"`
	ContainerRunning bool   `json:"container_running"`
	Registered       bool   `json:"registered"`
	// Offline lists the platform's registered runners for the repo that GitHub
	// reports as offline. A runner that is registered but not running here is
	// a ghost: it will never pick up a job and holds a registration slot.
	Offline   []string `json:"offline,omitempty"`
	Failures  int      `json:"consecutive_failures,omitempty"`
	LastError string   `json:"last_error,omitempty"`
	// Error is what Inspect could not read for this repo.
	Error string `json:"error,omitempty"`
}

// Inspect reads the runner state without changing anything. It is the doctor
// check's whole implementation: read settings, read the container labels, ask
// GitHub what it thinks is registered.
func (m *Manager) Inspect(ctx context.Context) (Report, error) {
	cfg := LoadConfig(ctx, m.Store, m.Node)
	rep := Report{Enabled: cfg.Enabled}
	if !cfg.Enabled {
		return rep, nil
	}
	if m.GitHub == nil {
		rep.Warnings = append(rep.Warnings, "no GitHub App is configured, so runners cannot be registered (pmcluster github app set)")
		return rep, nil
	}
	rep.AppConfigured = true

	if perms, err := m.GitHub.GrantedPermissions(ctx); err != nil {
		rep.Warnings = append(rep.Warnings, "could not read the App's granted permissions: "+err.Error())
	} else if levelRank(perms["administration"]) < levelRank("write") {
		rep.MissingPermission = RequiredPermission
	}

	live, err := m.liveServices(ctx)
	if err != nil {
		rep.Warnings = append(rep.Warnings, err.Error())
	}
	state := loadState(ctx, m.Store)

	for _, repo := range SortRepos(cfg.Repos) {
		rr := RepoReport{Repo: repo, RunnerName: RunnerName(cfg.Node, repo)}
		if svc, ok := live[repo]; ok {
			rr.ContainerRunning = svc.Replicas > 0
		}
		if entry, ok := state.Repos[repo]; ok {
			rr.Failures = entry.Failures
			rr.LastError = entry.LastError
		}
		list, err := m.GitHub.ListRunners(ctx, repo)
		if err != nil {
			rr.Error = err.Error()
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("list runners for %s: %v", repo, err))
			rep.Repos = append(rep.Repos, rr)
			continue
		}
		for _, r := range list {
			if r.Name == rr.RunnerName {
				rr.Registered = true
				if !strings.EqualFold(r.Status, "online") {
					rr.Offline = append(rr.Offline, r.Name)
				}
				continue
			}
			// Another platform runner for this repo (a previous node, a stale
			// container): named, so an operator can see the ghost rather than
			// wonder why the repo shows two runners.
			if strings.HasPrefix(r.Name, RunnerNamePrefix) {
				rr.Offline = append(rr.Offline, r.Name)
			}
		}
		rep.Repos = append(rep.Repos, rr)
	}
	return rep, nil
}

// levelRank orders GitHub permission levels the way the App's granted set is
// reported: absent < read < write < admin.
func levelRank(level string) int {
	switch strings.ToLower(level) {
	case "read":
		return 1
	case "write":
		return 2
	case "admin":
		return 3
	}
	return 0
}
