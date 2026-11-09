package runners

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// GitHub is the slice of the GitHub App surface the lifecycle needs. The
// production implementation is *githubapp.Local, which mints one cached
// installation token and uses it for every call — the App's private key never
// leaves the control plane, and a PAT is never involved.
type GitHub interface {
	// RunnerRegistrationToken mints a one-hour, single-use repo registration
	// token (POST /repos/{owner}/{repo}/actions/runners/registration-token).
	RunnerRegistrationToken(ctx context.Context, repo string) (string, error)
	// ListRunners returns the repo's registered runners
	// (GET /repos/{owner}/{repo}/actions/runners).
	ListRunners(ctx context.Context, repo string) ([]githubapp.Runner, error)
	// DeleteRunner de-registers one runner
	// (DELETE /repos/{owner}/{repo}/actions/runners/{id}).
	DeleteRunner(ctx context.Context, repo string, id int64) error
	// RepoPrivate reports whether the repository is private. A public repo is
	// refused: its fork PRs would run on a host-mounted docker socket.
	RepoPrivate(ctx context.Context, repo string) (bool, error)
	// GrantedPermissions reads the App's granted permission set.
	GrantedPermissions(ctx context.Context) (map[string]string, error)
}

// Deployer is the one-service stack lifecycle for a runner. DeployStackNoPrune
// is the right call, not DeployStack: the latter force-updates every service,
// which would kill a running job on every reconcile pass.
type Deployer interface {
	DeployStackNoPrune(ctx context.Context, name string, composeYAML []byte) error
	RemoveStack(ctx context.Context, name string) error
}

// ServiceLister is the runtime subset that discovers what actually exists.
type ServiceLister interface {
	ServiceList(ctx context.Context) ([]runtime.Service, error)
}

// Result is what one reconcile pass did, for the log line and the apply
// warning record.
type Result struct {
	Registered []string `json:"registered,omitempty"`
	Removed    []string `json:"removed,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

// Manager reconciles the runner lifecycle. It owns no goroutine: the control
// loop calls Reconcile on its normal cadence (reconcile_interval, 60s default).
type Manager struct {
	Store    *store.Store
	GitHub   GitHub
	Deployer Deployer
	Cluster  ServiceLister
	Log      zerolog.Logger
	Now      func() time.Time

	// RecycleCooldown is how long after a (re)registration a container that is
	// not running yet is left alone. A freshly created task sits at 0/1 while
	// it pulls the image; without this, every pass would mint another token and
	// redeploy, and the runner would never get to start.
	RecycleCooldown time.Duration

	// Node is this daemon's hostname, the stable half of the runner name.
	Node string
}

const defaultRecycleCooldown = 2 * time.Minute

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) cooldown() time.Duration {
	if m.RecycleCooldown > 0 {
		return m.RecycleCooldown
	}
	return defaultRecycleCooldown
}

// Reconcile converges the runner set: one running ephemeral container per
// configured private repo, and nothing for any other repo.
//
// Errors are returned only when the pass could not read its own state (the
// service list or the store). Everything else — a repo GitHub refuses, a stack
// that fails to deploy — is a WARNING, so one broken repo never stops the rest
// from converging.
func (m *Manager) Reconcile(ctx context.Context) (Result, error) {
	var res Result
	cfg := LoadConfig(ctx, m.Store, m.Node)
	state := loadState(ctx, m.Store)

	live, err := m.liveServices(ctx)
	if err != nil {
		return res, err
	}

	desired := map[string]bool{}
	if cfg.Enabled {
		for _, repo := range cfg.Repos {
			desired[repo] = true
		}
	}

	// 1. Teardown: a live runner service whose repo is no longer wanted.
	for _, repo := range sortedKeys(live) {
		if desired[repo] {
			continue
		}
		svc := live[repo]
		if m.teardown(ctx, repo, svc.Stack, &res) {
			delete(state.Repos, repo)
		}
	}

	// 2. Teardown: bookkeeping for a repo that is no longer wanted and whose
	//    service is already gone (removed by hand, or between daemon runs).
	//    Without this the GitHub runner would outlive its container forever.
	for _, repo := range sortedStateKeys(state) {
		if desired[repo] {
			continue
		}
		if m.teardown(ctx, repo, "", &res) {
			delete(state.Repos, repo)
		}
	}

	// 3. Ensure: one runner per desired repo.
	if cfg.Enabled {
		if m.GitHub == nil {
			res.Warnings = append(res.Warnings,
				"github_runners_enabled is on but no GitHub App is configured: runners cannot be registered. Configure one with `pmcluster github app set`.")
		} else {
			for _, repo := range SortRepos(cfg.Repos) {
				svc, exists := live[repo]
				m.ensure(ctx, cfg, repo, svc, exists, state, &res)
			}
		}
	}

	if err := saveState(ctx, m.Store, state); err != nil {
		return res, fmt.Errorf("save runner state: %w", err)
	}
	return res, nil
}

// ensure makes one repo's runner exist and run. svc is the live service (zero
// value when none) and ok reports whether it exists at all.
func (m *Manager) ensure(ctx context.Context, cfg Config, repo string, svc runtime.Service, exists bool, state State, res *Result) {
	name := RunnerName(cfg.Node, repo)
	stack := StackName(repo)

	// A running container is the whole desired state: the runner registered
	// itself with the token it was created with, and it is mid-job or idle
	// waiting for one. Touching it would kill the job.
	if exists && svc.Replicas > 0 {
		state.Repos[repo] = RepoState{Service: stack, Name: name, RegisteredAt: state.Repos[repo].RegisteredAt, Failures: 0, LastError: ""}
		return
	}

	// The stack exists but no task is running: either the ephemeral runner
	// just finished its one job, or the task is still starting. Leave a fresh
	// one alone; a finished one is recycled on a later pass.
	if exists {
		entry := state.Repos[repo]
		if at := entry.registeredAtTime(); !at.IsZero() && m.now().Sub(at) < m.cooldown() {
			m.Log.Debug().Str("repo", repo).Dur("cooldown", m.cooldown()).
				Msg("runners — container is not running yet and was registered recently; leaving it alone")
			return
		}
	}

	// The old container is gone (or never started). Its ephemeral runner
	// usually de-registered itself, but a crashed one does not — clear the
	// name so the new registration cannot collide with a ghost.
	if err := m.deleteRunnerNames(ctx, repo, name); err != nil {
		m.warn(res, repo, fmt.Errorf("clear stale runner %s: %w", name, err), state)
	}

	private, err := m.GitHub.RepoPrivate(ctx, repo)
	if err != nil {
		m.warn(res, repo, fmt.Errorf("read repository %s: %w", repo, err), state)
		return
	}
	if !private {
		m.warn(res, repo, fmt.Errorf("refusing to register a runner for %s: the repository is public, and a fork pull request would run its workflow on a container with /var/run/docker.sock mounted (root-equivalent on the host). Only private repositories are supported", repo), state)
		return
	}

	token, err := m.GitHub.RunnerRegistrationToken(ctx, repo)
	if err != nil {
		m.warn(res, repo, fmt.Errorf("mint a registration token for %s: %w", repo, err), state)
		return
	}

	compose, err := Render(Spec{
		Repo:        repo,
		RunnerName:  name,
		StackName:   stack,
		Image:       cfg.Image,
		Memory:      cfg.Memory,
		Token:       token,
		AllowLeader: cfg.AllowLeader,
	})
	if err != nil {
		m.warn(res, repo, fmt.Errorf("render runner for %s: %w", repo, err), state)
		return
	}

	// NoPrune: a full DeployStack force-updates every service, which would
	// restart a runner that is mid-job.
	if err := m.Deployer.DeployStackNoPrune(ctx, stack, compose); err != nil {
		m.warn(res, repo, fmt.Errorf("deploy runner stack for %s: %w", repo, err), state)
		return
	}

	state.Repos[repo] = RepoState{
		Service:      stack,
		Name:         name,
		RegisteredAt: m.now().Unix(),
	}
	res.Registered = append(res.Registered, repo)
	m.Log.Info().Str("repo", repo).Str("runner", name).Str("memory", cfg.Memory).
		Msg("runners — registered a runner")
}

// teardown removes a repo's container and de-registers its runner. It reports
// whether the repo is fully cleaned up; the caller only forgets the booking
// entry when it is, so a failed de-registration is retried next pass.
func (m *Manager) teardown(ctx context.Context, repo, stack string, res *Result) bool {
	clean := true

	if m.GitHub == nil {
		clean = false
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("removed the runner container for %s but could not de-register it: no GitHub App is configured. The runner may keep polling GitHub on this repo until it is deleted by hand.", repo))
	} else if err := m.deleteRunnerNames(ctx, repo, ""); err != nil {
		clean = false
		res.Warnings = append(res.Warnings, fmt.Sprintf("de-register runners for %s: %v", repo, err))
	}

	if stack != "" {
		if err := m.Deployer.RemoveStack(ctx, stack); err != nil {
			clean = false
			res.Warnings = append(res.Warnings, fmt.Sprintf("remove runner stack %s: %v", stack, err))
		} else {
			res.Removed = append(res.Removed, repo)
			m.Log.Info().Str("repo", repo).Str("stack", stack).Msg("runners — removed the runner container")
		}
	}
	return clean
}

// deleteRunnerNames removes the platform's runners for a repo: the exact name
// when given, otherwise every runner carrying the reserved pmc- prefix. An
// empty match is success.
func (m *Manager) deleteRunnerNames(ctx context.Context, repo, name string) error {
	if m.GitHub == nil {
		return nil
	}
	list, err := m.GitHub.ListRunners(ctx, repo)
	if err != nil {
		return err
	}
	var firstErr error
	for _, r := range list {
		match := r.Name == name
		if name == "" {
			match = strings.HasPrefix(r.Name, RunnerNamePrefix)
		}
		if !match {
			continue
		}
		if err := m.GitHub.DeleteRunner(ctx, repo, r.ID); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("delete runner %s (id %d): %w", r.Name, r.ID, err)
		}
	}
	return firstErr
}

// warn records one repo-level failure without aborting the pass: an apply
// warning names it in the harness and the doctor's "last apply" check, which is
// where an unattended reconcile's failures have to surface.
func (m *Manager) warn(res *Result, repo string, err error, state State) {
	entry := state.Repos[repo]
	entry.Failures++
	entry.LastError = err.Error()
	if entry.Service == "" {
		entry.Service = StackName(repo)
	}
	state.Repos[repo] = entry
	res.Warnings = append(res.Warnings, err.Error())
	m.Log.Warn().Err(err).Str("repo", repo).Int("consecutive_failures", entry.Failures).
		Msg("runners — repo did not converge")
}

// liveServices maps repo -> live runner service, discovered by label. This is
// the "actual" half of reconcile: a container started by an earlier daemon (or
// by hand) is visible without trusting any table.
func (m *Manager) liveServices(ctx context.Context) (map[string]runtime.Service, error) {
	out := map[string]runtime.Service{}
	if m.Cluster == nil {
		return out, nil
	}
	svcs, err := m.Cluster.ServiceList(ctx)
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	for _, s := range svcs {
		if s.Labels[LabelRunner] != "true" {
			continue
		}
		repo := s.Labels[LabelRepo]
		if repo == "" {
			continue
		}
		out[repo] = s
	}
	return out, nil
}

func sortedKeys(m map[string]runtime.Service) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedStateKeys(s State) []string {
	out := make([]string, 0, len(s.Repos))
	for k := range s.Repos {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
