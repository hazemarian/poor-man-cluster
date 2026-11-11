package runners

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// The console's runner view. It is built from two observations, deliberately:
// GitHub's org runner list is authoritative for "is this runner registered and
// what is it doing", and the local container is authoritative for "is something
// running here". Neither alone is enough — a registration with no container is a
// ghost, and a container whose registration was deleted is a job that will never
// arrive.

// Runner table states. They are the CHECK values of
// migrations/0028_github_runners.sql.
const (
	StateStarting = "starting"
	StateIdle     = "idle"
	StateBusy     = "busy"
	StateOffline  = "offline"
	StateFailed   = "failed"
)

// ModeEphemeral is the only mode the platform creates: one job, then exit.
const ModeEphemeral = "ephemeral"

// ErrRunnerBusy is returned by Kill/Restart when the runner is executing a job
// and the caller did not confirm. Killing it cancels that workflow run.
var ErrRunnerBusy = errors.New("runner is busy: killing it will fail the job it is running; resend with confirm=true")

// ErrRunnerNotFound is the API's 404. It is the store's sentinel by another
// name so callers do not import the store for it.
var ErrRunnerNotFound = store.ErrGitHubRunnerNotFound

// KillOutcome is what one kill/restart did. It is returned even when the
// de-registration failed: the caller needs to know which half worked.
type KillOutcome struct {
	Name             string `json:"name"`
	Org              string `json:"org"`
	Node             string `json:"node"`
	ContainerRemoved bool   `json:"container_removed"`
	Deregistered     bool   `json:"deregistered"`
	DeregisterError  string `json:"deregister_error,omitempty"`
	Held             bool   `json:"held"`
	Registered       bool   `json:"registered,omitempty"`
	// RegistrationError names the half that did not happen on a restart.
	RegistrationError string `json:"registration_error,omitempty"`
}

// Runners returns the console rows: one per selected node when the feature is
// on, merged with what GitHub reports for that name, and with the local
// container when the row belongs to this node.
func (m *Manager) Runners(ctx context.Context) ([]store.GitHubRunner, error) {
	if m.Store == nil {
		return nil, nil
	}
	cfg := LoadConfig(ctx, m.Store, m.Node)
	rows, err := m.Store.ListGitHubRunners(ctx, 200)
	if err != nil {
		return nil, err
	}
	prev := make(map[string]store.GitHubRunner, len(rows))
	for _, r := range rows {
		prev[r.Name] = r
	}
	if !cfg.Enabled || cfg.Org == "" {
		return rows, nil
	}

	// GitHub's list for the org, when it answers. An unreachable GitHub is not
	// an empty runner list: the rows keep their last known state and their age
	// stops advancing, which the panel shows as staleness.
	var remote map[string]bool // name -> busy
	remoteOK := false
	if m.GitHub != nil {
		list, lerr := m.GitHub.ListRunners(ctx, cfg.Org)
		if lerr == nil {
			remoteOK = true
			remote = make(map[string]bool, len(list))
			for _, r := range list {
				remote[r.Name] = r.Busy
			}
		} else {
			m.Log.Debug().Err(lerr).Str("org", cfg.Org).Msg("runners — GitHub unreachable; the panel keeps the last known state")
		}
	}

	state := loadState(ctx, m.Store)
	now := m.now()
	out := make([]store.GitHubRunner, 0, len(cfg.Nodes))
	for _, node := range cfg.Nodes {
		name := RunnerName(node)
		row := prev[name]
		row.Name, row.Org, row.Node, row.Mode = name, cfg.Org, node, ModeEphemeral
		// A node whose RAM cannot hold the configured cap is refused before
		// anything is started, so its row says so instead of looking idle. State
		// stays `offline` — it is the truth about the container — and Reason is
		// what makes the row actionable. The cap is resolved for THIS node, so
		// a refused row names the override the operator actually set.
		if refused, rerr := m.memoryRefusal(ctx, node, cfg.CapFor(node)); refused {
			row.State = StateOffline
			row.Reason = rerr.Error()
			row.ContainerID = ""
			out = append(out, row)
			continue
		}
		if row.RegisteredAt == 0 {
			if e, ok := state.entry(name); ok {
				row.RegisteredAt = e.RegisteredAt
			}
		}
		// The local container, when this row is this node's.
		running := false
		if strings.EqualFold(node, m.Node) {
			if c, ok, cerr := m.inspect(ctx, name); cerr == nil && ok {
				running = c.Running()
				row.ContainerID = c.ID
			}
		}
		busy, known := remote[name]
		row.State = deriveRunnerState(deriveInput{
			prev: row.State,
			busy: busy,
			// ghOK is "GitHub answered at all": a name it does not list is a
			// runner it does not know, which is different from an unreachable
			// GitHub (where the last known state has to stand).
			ghOK:         remoteOK,
			registered:   known,
			running:      running,
			held:         state.held(name),
			failures:     state.Runners[name].Failures,
			registeredAt: time.Unix(row.RegisteredAt, 0),
			now:          now,
			cooldown:     m.cooldown(),
		})
		if remoteOK {
			row.LastSeen = now.Unix()
		}
		out = append(out, row)
	}
	return out, nil
}

// Enabled reports the feature switch from the one source of truth
// (github_runners_enabled). The panel's empty state needs it on the same
// response as the list, so the two cannot disagree.
func (m *Manager) Enabled(ctx context.Context) bool {
	return LoadConfig(ctx, m.Store, m.Node).Enabled
}

// Kill removes the local runner container (when this node owns it) and
// de-registers the runner from GitHub, then HOLDS the name so no reconcile pass
// re-registers it. Without the hold a Kill button would be undone by the next
// pass sixty seconds later.
//
// confirmBusy must be true for a runner in the `busy` state: that kill cancels
// the running workflow, and the refusal is the API's way of making the caller
// say so.
func (m *Manager) Kill(ctx context.Context, name string, confirmBusy bool) (KillOutcome, error) {
	if m.Store == nil {
		return KillOutcome{}, fmt.Errorf("no store configured")
	}
	row, err := m.findRunner(ctx, name)
	if err != nil {
		return KillOutcome{}, err
	}
	if row.State == StateBusy && !confirmBusy {
		return KillOutcome{Name: name, Org: row.Org, Node: row.Node}, ErrRunnerBusy
	}

	out := KillOutcome{Name: name, Org: row.Org, Node: row.Node}
	if m.Docker != nil {
		if _, exists, ierr := m.Docker.Inspect(ctx, name); ierr == nil && exists {
			if rerr := m.Docker.Remove(ctx, name); rerr != nil {
				// Nothing was killed: the container is still running, so this is
				// the one failure that is a 500.
				return out, fmt.Errorf("remove the runner container %s: %w", name, rerr)
			}
			out.ContainerRemoved = true
		}
	}

	if m.GitHub == nil {
		out.DeregisterError = "no GitHub App is configured, so the runner could not be de-registered"
	} else if strings.TrimSpace(row.Org) != "" {
		if derr := m.deleteRunnerByName(ctx, row.Org, name); derr != nil {
			// The container is gone; the registration is not. Report the half
			// that worked instead of pretending the whole kill failed.
			out.DeregisterError = derr.Error()
		} else {
			out.Deregistered = true
		}
	}

	state := loadState(ctx, m.Store)
	delete(state.Runners, name)
	cfg := LoadConfig(ctx, m.Store, m.Node)
	if cfg.Enabled && strings.EqualFold(cfg.Org, row.Org) {
		state.Killed[name] = m.now().Unix()
		out.Held = true
	}
	if err := saveState(ctx, m.Store, state); err != nil {
		return out, fmt.Errorf("save runner hold: %w", err)
	}

	// The row is current state, not history: a killed runner is not a row with
	// state=offline, it is a runner whose container and registration are gone.
	// The hold above is what makes the reconcile leave it alone, and the panel
	// derives `offline` from the hold.
	if err := m.Store.DeleteGitHubRunner(ctx, name); err != nil {
		return out, fmt.Errorf("clear runner row: %w", err)
	}
	m.Log.Info().Str("org", row.Org).Str("runner", name).Bool("held", out.Held).
		Msg("runners — killed by an operator")
	return out, nil
}

// HeldName reports whether an operator stopped this runner from the console.
//
// The roster lives in the leader's store, and a worker cannot read it — so the
// leader resolves this into the assignment instead of expecting the node to
// know. Without that hop a kill for a remote node was recorded and never
// delivered, and the node recreated the runner on its next pass.
func (m *Manager) HeldName(ctx context.Context, name string) bool {
	if m == nil || m.Store == nil {
		return false
	}
	return loadState(ctx, m.Store).held(name)
}

// findRunner resolves a runner by name from the live view first (GitHub's state
// is what makes a busy kill confirmable), then from the local row cache. A name
// that neither knows is a 404.
func (m *Manager) findRunner(ctx context.Context, name string) (*store.GitHubRunner, error) {
	view, verr := m.Runners(ctx)
	if verr == nil {
		for i := range view {
			if view[i].Name == name {
				return &view[i], nil
			}
		}
	}
	row, err := m.Store.GitHubRunner(ctx, name)
	if err != nil {
		if errors.Is(err, ErrRunnerNotFound) {
			return nil, ErrRunnerNotFound
		}
		return nil, err
	}
	return row, nil
}

// Restart drops the kill hold and registers the runner again immediately: a kill
// followed by a registration, not a request for the next reconcile to notice.
func (m *Manager) Restart(ctx context.Context, name string, confirmBusy bool) (KillOutcome, error) {
	out, err := m.Kill(ctx, name, confirmBusy)
	if err != nil {
		return out, err
	}
	cfg := LoadConfig(ctx, m.Store, m.Node)
	state := loadState(ctx, m.Store)
	state.dropHold(name)
	if err := saveState(ctx, m.Store, state); err != nil {
		return out, fmt.Errorf("save runner hold: %w", err)
	}
	switch {
	case cfg.Enabled && !cfg.WatchesNode():
		out.RegistrationError = fmt.Sprintf("node %s is not in github_runner_nodes, so no runner will start here", m.Node)
		return out, nil
	case m.GitHub == nil:
		out.RegistrationError = "no GitHub App is configured, so the runner cannot be registered"
		return out, nil
	}
	if err := m.ensureGroup(ctx, cfg); err != nil {
		out.RegistrationError = err.Error()
		return out, nil
	}
	var res Result
	m.ensure(ctx, cfg, Container{}, false, state, &res)
	if len(res.Registered) == 1 {
		out.Registered = true
	} else if len(res.Warnings) > 0 {
		out.RegistrationError = strings.Join(res.Warnings, "; ")
	}
	if err := saveState(ctx, m.Store, state); err != nil {
		return out, fmt.Errorf("save runner state: %w", err)
	}
	return out, nil
}

// syncRow writes THIS node's row (it never prunes other nodes' rows: each node
// owns its own name, and a shared store would otherwise have one node deleting
// another's row on every pass).
func (m *Manager) syncRow(ctx context.Context, cfg Config, want bool, state State, res *Result) error {
	if m.Store == nil {
		return nil
	}
	name := RunnerName(cfg.Node)
	if !cfg.Enabled {
		// The feature is off: the panel shows its disabled empty state, and no
		// row may linger as a ghost.
		if err := m.Store.DeleteGitHubRunner(ctx, name); err != nil {
			return fmt.Errorf("clear runner row: %w", err)
		}
		return nil
	}
	row := store.GitHubRunner{
		Name: name,
		Org:  cfg.Org,
		Node: cfg.Node,
		Mode: ModeEphemeral,
	}
	if e, ok := state.entry(name); ok {
		row.RegisteredAt = e.RegisteredAt
	}
	if c, ok, err := m.inspect(ctx, name); err == nil && ok {
		row.ContainerID = c.ID
		row.State = StateIdle
		if !c.Running() {
			row.State = StateStarting
		}
	}
	if state.held(name) {
		row.State = StateOffline
	}
	if row.State == "" {
		row.State = StateStarting
	}
	if !want {
		row.State = StateOffline
	}
	row.LastSeen = m.now().Unix()
	return m.Store.PutGitHubRunner(ctx, row)
}

// deriveInput is one runner's observation, kept as a struct because six bare
// booleans and strings at a call site are how a state machine gets inverted.
type deriveInput struct {
	prev         string
	busy         bool
	ghOK         bool
	registered   bool
	running      bool
	held         bool
	failures     int
	registeredAt time.Time
	now          time.Time
	cooldown     time.Duration
}

// deriveRunnerState turns the two observations into one state. The order is the
// priority: held or repeatedly failing first (they are true regardless of what
// docker or GitHub say), then what GitHub reports, then the local container, and
// only then the last known state — the panel never invents one.
func deriveRunnerState(in deriveInput) string {
	if in.held {
		return StateOffline
	}
	if in.failures >= 3 {
		return StateFailed
	}
	if in.ghOK {
		// GitHub is authoritative for a registered runner: it is idle unless it
		// says busy. "No local container" is not evidence of offline — the
		// container may be on another node, or invisible to this daemon.
		if in.busy {
			return StateBusy
		}
		if in.registered {
			return StateIdle
		}
		// GitHub does not list it yet: a container that just started (or a
		// fresh registration inside the cooldown) is starting, not offline —
		// "offline" during an image pull would be a lie.
		if in.running || (!in.registeredAt.IsZero() && in.now.Sub(in.registeredAt) < in.cooldown) {
			return StateStarting
		}
		return StateOffline
	}
	if in.running {
		return StateIdle
	}
	if !in.registeredAt.IsZero() && in.now.Sub(in.registeredAt) < in.cooldown {
		return StateStarting
	}
	if in.prev != "" {
		return in.prev
	}
	return StateStarting
}
