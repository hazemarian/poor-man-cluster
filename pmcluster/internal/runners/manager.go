package runners

import (
	"context"
	"errors"
	"fmt"
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
	// RunnerRegistrationToken mints a one-hour, single-use ORG registration
	// token (POST /orgs/{org}/actions/runners/registration-token).
	RunnerRegistrationToken(ctx context.Context, org string) (string, error)
	// ListRunners returns the organisation's registered runners
	// (GET /orgs/{org}/actions/runners).
	ListRunners(ctx context.Context, org string) ([]githubapp.Runner, error)
	// DeleteRunner de-registers one runner
	// (DELETE /orgs/{org}/actions/runners/{id}).
	DeleteRunner(ctx context.Context, org string, id int64) error
	// EnsureRunnerGroup creates the org runner group with visibility
	// `selected` when it is absent and refuses an existing group whose
	// visibility is anything else. It is the security boundary of the whole
	// feature: the group decides which repositories may schedule onto a runner
	// that can reach a docker daemon.
	EnsureRunnerGroup(ctx context.Context, org, group string) (githubapp.RunnerGroup, error)
	// LatestRunURL is the organisation's newest workflow run: repository and
	// web URL. Empty when the org has no runs. Best-effort — it needs
	// actions:read, which is not part of the runner permission set.
	LatestRunURL(ctx context.Context, org string) (repo, url string, err error)
	// GrantedPermissions reads the App's granted permission set.
	GrantedPermissions(ctx context.Context) (map[string]string, error)
}

// NodeLister is the runtime subset the role check needs: a runner must never run
// on a manager, and the leader is a manager.
type NodeLister interface {
	NodeList(ctx context.Context) ([]runtime.Node, error)
}

// The refusals. Each is a hard error, not a warning: a runner that cannot be
// registered, cannot be scoped, or would land on the control plane must not be
// started at all.
var (
	// ErrNoApp is "enabled without a GitHub App".
	ErrNoApp = errors.New("github_runners_enabled is on but no GitHub App is configured: a runner registers with an App installation token, which needs the App's administration: write ORGANIZATION permission — configure one with `pmcluster github app set`, or turn the feature off")

	// ErrNoOrg is "enabled without a target organisation".
	ErrNoOrg = errors.New("github_runners_enabled is on but github_runner_org is empty: an org-level runner registers into an organisation, so set github_runner_org (for example acme)")

	// ErrNoNodes is "enabled with nowhere to run".
	ErrNoNodes = errors.New("github_runners_enabled is on but github_runner_nodes is empty: a runner is a local container on a worker, so at least one worker hostname must be selected (the Runners panel has a node picker)")

	// ErrManagerNode is a manager (or the leader) in the selection. A build on
	// the control plane's 1-vCPU manager marks the node Down and takes the
	// cluster API with it — that happened on the live cluster with the telemetry
	// loop, which is why this is a refusal rather than a warning.
	ErrManagerNode = errors.New("refusing to run a runner on a manager node: the manager is the control plane (1 vCPU), and a build there marks the node Down and takes the cluster API with it — select a worker")
)

// Result is what one reconcile pass did, for the log line and the apply
// warning record.
type Result struct {
	Registered []string `json:"registered,omitempty"`
	Removed    []string `json:"removed,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

// Manager reconciles this node's runner. It owns no goroutine: the daemon calls
// Reconcile on a fixed cadence on EVERY node, and each node decides for itself
// whether it is selected (see Config.WatchesNode).
type Manager struct {
	Store *store.Store
	// GitHub is nil when no App is configured; the feature then refuses.
	GitHub GitHub
	// Docker is the local container runtime.
	Docker LocalDocker
	// Nodes reads the swarm node list for the role check. Optional: nil skips the
	// role check (tests), the daemon always wires it.
	Nodes NodeLister
	Log   zerolog.Logger
	Now   func() time.Time

	// RecycleCooldown is how long after a (re)registration a container that is
	// not running yet is left alone, so a container that is still pulling its
	// image is not re-created every pass.
	RecycleCooldown time.Duration

	// Node is this daemon's hostname: the node that decides whether it runs a
	// runner, and the stable half of the runner name.
	Node string

	// NodeMemory returns a node's total RAM in MiB (known=false without a recent
	// sample). Optional: nil means the size check cannot be made here, and the
	// reconcile then relies on the leader's assignment refusal — the two use the
	// same rule (CheckMemoryFit), so neither can be stricter than the other.
	NodeMemory func(ctx context.Context, node string) (mib int64, known bool)
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

// Reconcile converges THIS node's runner: one local container and one org
// registration when the node is selected, nothing when it is not. It is also the
// teardown path — a node removed from the list, or the whole feature turned off,
// removes the local container and de-registers the name it owns.
//
// Errors are returned for refusals and for a pass that could not read its own
// state. A failed deploy or an unreachable GitHub is a WARNING, so a transient
// outage does not stop teardown from converging.
func (m *Manager) Reconcile(ctx context.Context) (Result, error) {
	var res Result
	cfg := LoadConfig(ctx, m.Store, m.Node)
	state := loadState(ctx, m.Store)
	name := RunnerName(cfg.Node)

	var refusal error
	want := false
	if cfg.Enabled {
		switch {
		case m.GitHub == nil:
			refusal = ErrNoApp
		case strings.TrimSpace(cfg.Org) == "":
			refusal = ErrNoOrg
		case len(cfg.Nodes) == 0:
			refusal = ErrNoNodes
		}
		if refusal == nil && cfg.WatchesNode() {
			if err := m.checkNodeRole(ctx); err != nil {
				// A manager in the list is refused on every node, not only the
				// manager itself: the operator must see one named error rather
				// than "the worker refused, the manager refused".
				refusal = err
			}
		}
		if refusal == nil && cfg.WatchesNode() {
			// The size rule, on the node that would run the container. A worker
			// normally gets this from the leader's assignment (it reads no
			// settings); this is the same rule for the node whose own store is
			// current — the leader with github_runner_allow_leader, and the
			// single-node cluster.
			if refused, rerr := m.memoryRefusal(ctx, m.Node, cfg.Memory); refused {
				refusal = fmt.Errorf("refused: %w", rerr)
			}
		}
		if refusal == nil && cfg.WatchesNode() {
			// The group is verified before a container is allowed to exist, and
			// on every pass: a visibility flipped to `all` on the GitHub side
			// must stop this runner, not wait for the next re-registration.
			if gerr := m.ensureGroup(ctx, cfg); gerr != nil {
				refusal = gerr
			} else {
				want = true
			}
		}
	}

	local, exists, err := m.inspect(ctx, name)
	if err != nil {
		return res, err
	}

	// 1. Teardown: this node runs a runner it should not. Only THIS node's name is
	//    ever touched — another node's runner is that node's to stop, and a shared
	//    store must not turn a node leaving the list into a cluster-wide sweep.
	handled := false
	if !want && exists {
		org := cfg.Org
		if e, ok := state.entry(name); ok && e.Org != "" {
			// The entry remembers the org the runner was registered in, which is
			// what de-registration needs after an org change.
			org = e.Org
		}
		if m.teardown(ctx, org, name, &res) {
			delete(state.Runners, name)
			delete(state.Killed, name)
		}
		handled = true
	}

	// 2. Teardown bookkeeping: the container is gone but this node still holds a
	//    registration record for it (removed by hand, or a previous pass that could
	//    not reach GitHub). Without this a half-disable leaves a runner that keeps
	//    polling GitHub forever. Skipped when step 1 already asked GitHub: asking
	//    twice would log the same failure twice.
	if !want && !handled {
		if e, ok := state.entry(name); ok {
			if m.teardown(ctx, e.Org, name, &res) {
				delete(state.Runners, name)
			}
		}
	}

	// 3. Ensure: the runner, unless an operator killed it from the console. A
	//    hold survives passes until Restart or until the node leaves the list —
	//    otherwise the Kill button would be undone within a minute.
	if want {
		if state.held(name) {
			m.Log.Debug().Str("runner", name).Msg("runners — held after an operator kill; not re-registering")
		} else {
			m.ensure(ctx, cfg, local, exists, state, &res)
		}
	}

	if err := m.syncRow(ctx, cfg, want, state, &res); err != nil {
		res.Warnings = append(res.Warnings, err.Error())
	}
	if err := saveState(ctx, m.Store, state); err != nil {
		return res, fmt.Errorf("save runner state: %w", err)
	}
	if refusal != nil {
		return res, refusal
	}
	return res, nil
}

// checkNodeRole refuses the pass when any selected node is a manager, and when
// this node is the leader. It needs the live node list, which is why it is here
// and not in the settings validator (the console's picker makes the same check
// against the same list).
func (m *Manager) checkNodeRole(ctx context.Context) error {
	if m.Nodes == nil {
		return nil
	}
	nodes, err := m.Nodes.NodeList(ctx)
	if err != nil {
		// The role check is a safety net; not being able to read the node list
		// is not a reason to start a runner on an unknown node.
		return fmt.Errorf("read the node list to verify runner placement: %w", err)
	}
	// One implementation, shared with the assignment resolver the leader serves
	// to workers: a placement this refuses is refused there too.
	return CheckPlacement(LoadConfig(ctx, m.Store, m.Node), nodes)
}

func roleWord(n runtime.Node) string {
	if n.IsLeader {
		return "leader"
	}
	if strings.EqualFold(n.Role, "manager") {
		return "manager"
	}
	return "worker"
}

// ensureGroup verifies (and creates or re-scopes) the org runner group. A
// visibility that is not `selected` comes back as a refusal naming the reason.
func (m *Manager) ensureGroup(ctx context.Context, cfg Config) error {
	if err := ensureGroupSelected(ctx, m.GitHub, cfg.Org, cfg.Group); err != nil {
		return err
	}
	return nil
}

// inspect returns the local runner container (zero value, ok=false when absent).
func (m *Manager) inspect(ctx context.Context, name string) (Container, bool, error) {
	if m.Docker == nil {
		return Container{}, false, nil
	}
	c, ok, err := m.Docker.Inspect(ctx, name)
	if err != nil {
		return Container{}, false, fmt.Errorf("inspect runner container %s: %w", name, err)
	}
	return c, ok, nil
}

// ensure creates this node's runner if it is not already running. c is the local
// container (zero value when absent); exists reports whether it exists at all.
func (m *Manager) ensure(ctx context.Context, cfg Config, c Container, exists bool, state State, res *Result) {
	name := RunnerName(cfg.Node)

	// A running container is the whole desired state: it registered itself with
	// the token it was created with, and it is mid-job or idle waiting for one.
	if exists && c.Running() {
		prev := state.Runners[name]
		state.Runners[name] = RunnerState{Name: name, Org: cfg.Org, RegisteredAt: prev.RegisteredAt, Failures: 0, LastError: ""}
		return
	}
	// The container exists but is not running: either the ephemeral runner just
	// finished its one job, or it is still starting. Leave a fresh one alone.
	if exists {
		if at := state.Runners[name].registeredAtTime(); !at.IsZero() && m.now().Sub(at) < m.cooldown() {
			m.Log.Debug().Str("runner", name).Dur("cooldown", m.cooldown()).
				Msg("runners — container is not running yet and was registered recently; leaving it alone")
			return
		}
		// A finished ephemeral runner is removed before its replacement: the
		// name is unique per node, and `docker run` refuses a duplicate name.
		if err := m.Docker.Remove(ctx, name); err != nil {
			m.warn(res, cfg.Org, name, fmt.Errorf("remove the finished runner container %s: %w", name, err), state)
			return
		}
	}

	// Its ephemeral runner usually de-registered itself, but a crashed one does
	// not — clear the exact name so the new registration cannot collide with a
	// ghost. Only this node's own name is ever deleted.
	if err := m.deleteRunnerByName(ctx, cfg.Org, name); err != nil {
		m.warn(res, cfg.Org, name, fmt.Errorf("clear stale runner %s: %w", name, err), state)
	}

	token, err := m.GitHub.RunnerRegistrationToken(ctx, cfg.Org)
	if err != nil {
		m.warn(res, cfg.Org, name, fmt.Errorf("mint a registration token for org %s: %w", cfg.Org, err), state)
		return
	}
	argv, err := Spec{
		Org:         cfg.Org,
		Group:       cfg.Group,
		RunnerName:  name,
		Image:       cfg.Image,
		Memory:      cfg.Memory,
		Token:       token,
		SocketProxy: cfg.SocketProxy,
		SocketPath:  cfg.SocketPath,
	}.Argv()
	if err != nil {
		m.warn(res, cfg.Org, name, fmt.Errorf("render runner for %s: %w", cfg.Org, err), state)
		return
	}
	if err := m.Docker.Run(ctx, argv); err != nil {
		m.warn(res, cfg.Org, name, fmt.Errorf("start the runner container for %s: %w", cfg.Org, err), state)
		return
	}
	state.Runners[name] = RunnerState{Name: name, Org: cfg.Org, RegisteredAt: m.now().Unix()}
	res.Registered = append(res.Registered, cfg.Org)
	mesh := "socket proxy"
	if !cfg.SocketProxy {
		mesh = "raw docker socket (debug fallback)"
	}
	m.Log.Info().Str("org", cfg.Org).Str("runner", name).Str("group", cfg.Group).
		Str("memory", cfg.Memory).Str("docker_access", mesh).
		Msg("runners — registered a runner on this node")
}

// teardown removes this node's runner container (if it has one) and de-registers
// the name. It reports whether everything is cleaned up; the caller keeps the
// bookkeeping entry when the de-registration is unconfirmed, so a later pass
// retries it. The container is always removed, even with GitHub unreachable:
// stopping local docker access must not depend on an API.
func (m *Manager) teardown(ctx context.Context, org, name string, res *Result) bool {
	clean := true
	if name != "" && m.GitHub != nil {
		if err := m.deleteRunnerByName(ctx, org, name); err != nil {
			clean = false
			res.Warnings = append(res.Warnings, fmt.Sprintf("de-register %s in org %s: %v", name, org, err))
		}
	} else if name != "" {
		clean = false
		res.Warnings = append(res.Warnings, fmt.Sprintf("removed the runner container but could not de-register %s: no GitHub App is configured, so the registration is unconfirmed and it may keep polling GitHub until it is deleted by hand", name))
	}
	if m.Docker != nil {
		if _, exists, err := m.Docker.Inspect(ctx, name); err != nil {
			clean = false
			res.Warnings = append(res.Warnings, fmt.Sprintf("inspect runner container %s: %v", name, err))
		} else if exists {
			if err := m.Docker.Remove(ctx, name); err != nil {
				clean = false
				res.Warnings = append(res.Warnings, fmt.Sprintf("remove runner container %s: %v", name, err))
			} else {
				res.Removed = append(res.Removed, name)
				m.Log.Info().Str("runner", name).Msg("runners — removed the runner container")
			}
		}
	}
	return clean
}

// DeregisterName removes one registration by name in the given organisation.
//
// It exists for the assignment endpoint: a node the leader no longer assigns must
// have its registration removed, and the WORKER cannot do it (it has no App
// installation — giving each worker one would be a far wider credential than this
// feature needs). Only the exact name the platform derives for that node is ever
// touched, so the operator's hand-made runner is never a candidate.
func (m *Manager) DeregisterName(ctx context.Context, org, name string) error {
	return m.deleteRunnerByName(ctx, org, name)
}

// deleteRunnerByName removes exactly one registration. An empty match is success
// (idempotent), and a name is required — there is deliberately no
// "delete everything with the pmc- prefix" mode, because an org can hold a
// pmc-named runner this platform does not own (the operator's hand-made one).
func (m *Manager) deleteRunnerByName(ctx context.Context, org, name string) error {
	if m.GitHub == nil {
		return nil
	}
	if name == "" {
		return errors.New("runner name is empty; refusing to guess which registration to delete")
	}
	if strings.TrimSpace(org) == "" {
		return errors.New("organisation is empty; refusing to list runners without a scope")
	}
	list, err := m.GitHub.ListRunners(ctx, org)
	if err != nil {
		return err
	}
	for _, r := range list {
		if r.Name != name {
			continue
		}
		if err := m.GitHub.DeleteRunner(ctx, org, r.ID); err != nil {
			return fmt.Errorf("delete runner %s (id %d): %w", r.Name, r.ID, err)
		}
	}
	return nil
}

// warn records one failure without aborting the pass: an apply warning names it
// in the harness and the doctor's "last apply" check, which is where an
// unattended reconcile's failures have to surface.
func (m *Manager) warn(res *Result, org, name string, err error, state State) {
	entry := state.Runners[name]
	entry.Failures++
	entry.LastError = err.Error()
	entry.Name, entry.Org = name, org
	state.Runners[name] = entry
	res.Warnings = append(res.Warnings, err.Error())
	m.Log.Warn().Err(err).Str("org", org).Int("consecutive_failures", entry.Failures).
		Msg("runners — did not converge")
}
