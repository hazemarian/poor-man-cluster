package runners

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"
)

// Agent runs this node's runner from the leader's ASSIGNMENT instead of from the
// store. It exists because a swarm WORKER cannot read cluster settings at all
// (Docker configs are manager-only), so the settings-based Manager can never
// answer "am I selected?" on the only nodes allowed to host a runner.
//
// It deliberately has no store and no GitHub client:
//
//   - the DECISION (enabled, org, group, memory, socket path, interval) arrives
//     in the assignment, already resolved by the leader;
//   - the registration token arrives with it (single-use, one hour) because an
//     ephemeral runner cannot be created without one;
//   - de-registration stays on the leader, which is the only side holding the
//     App installation. A worker that could de-register would need an
//     installation token, a far wider credential than this feature needs.
//
// Bookkeeping is a local file, not a settings row: the node's store either does
// not exist (a worker never opens one) or is the leader's own. The file is the
// evidence `pmcluster doctor` reads on a worker.
type Agent struct {
	// Node is this daemon's hostname (the stable half of the runner name).
	Node string
	// Docker is the local container runtime.
	Docker LocalDocker
	// StatePath is where the applied assignment is recorded (0600). Empty keeps
	// it in memory only (tests).
	StatePath string
	// RecycleCooldown is how long a not-yet-running container is left alone, so
	// a container that is still pulling its image is not re-created every pass.
	RecycleCooldown time.Duration
	Now             func() time.Time
	Log             zerolog.Logger
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Agent) cooldown() time.Duration {
	if a.RecycleCooldown > 0 {
		return a.RecycleCooldown
	}
	return defaultRecycleCooldown
}

// AgentRecord is what this node last applied — the doctor's view on a worker,
// and the reason a restart re-applies the same decision without re-registering.
type AgentRecord struct {
	Node        string `json:"node"`
	Enabled     bool   `json:"enabled"`
	Org         string `json:"org"`
	Group       string `json:"group"`
	RunnerName  string `json:"runner_name"`
	SocketPath  string `json:"socket_path"`
	SocketProxy bool   `json:"socket_proxy"`
	// DinD: the job gets a docker daemon of its own inside a privileged sibling
	// container, instead of the platform's filtered socket. It is the mode that
	// makes ordinary CI work (`docker compose`, testcontainers, exec), and it is
	// carried in the assignment so a worker never decides it for itself.
	DinD      bool   `json:"dind"`
	Container string `json:"container"`
	// Memory is the cap this node last applied (or was refused for). It is what
	// `pmcluster doctor` compares against THIS machine's RAM on a worker, which
	// has no store and cannot ask the leader.
	Memory string `json:"memory,omitempty"`
	// Refused/RefusedReason are the leader's size refusal, recorded so a worker's
	// own doctor can report it without a second channel.
	Refused       bool   `json:"refused,omitempty"`
	RefusedReason string `json:"refused_reason,omitempty"`
	RegisteredAt  int64  `json:"registered_at,omitempty"`
	AppliedAt     int64  `json:"applied_at"`
	LastError     string `json:"last_error,omitempty"`
	// FetchedAt and Source are when and where this node last READ its
	// assignment. They are what `pmcluster doctor` reports on a worker, because
	// "did this node ever reach the leader" is otherwise unanswerable there.
	FetchedAt int64  `json:"fetched_at,omitempty"`
	Source    string `json:"source,omitempty"`
}

// RecordFetch records that an assignment was read from source at now, keeping the
// rest of the record. The token is never written: it is single-use, and a
// credential on disk is a credential to leak.
func (a *Agent) RecordFetch(source string, at time.Time) {
	rec, ok := a.Record()
	if !ok {
		rec = AgentRecord{Node: a.Node, RunnerName: RunnerName(a.Node)}
	}
	rec.FetchedAt = at.Unix()
	rec.Source = source
	a.save(rec)
}

// Record returns the last applied state, if any was written.
func (a *Agent) Record() (AgentRecord, bool) {
	if a.StatePath == "" {
		return AgentRecord{}, false
	}
	b, err := os.ReadFile(a.StatePath)
	if err != nil {
		return AgentRecord{}, false
	}
	var rec AgentRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return AgentRecord{}, false
	}
	return rec, true
}

// save writes the record atomically with 0600: it names the node and the org,
// which is not a credential but is nobody else's business on a shared host.
func (a *Agent) save(rec AgentRecord) {
	if a.StatePath == "" {
		return
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(a.StatePath), 0o700); err != nil {
		a.Log.Debug().Err(err).Msg("runners — could not create the agent state directory")
		return
	}
	tmp := a.StatePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		a.Log.Debug().Err(err).Msg("runners — could not write the agent state file")
		return
	}
	if err := os.Rename(tmp, a.StatePath); err != nil {
		a.Log.Debug().Err(err).Msg("runners — could not replace the agent state file")
	}
}

// Apply converges this node's runner onto the leader's decision: start it when
// the assignment is enabled, stop and remove it when it is not. It never talks to
// GitHub — that is the leader's half (see the type comment).
//
// Errors are returned for a pass that could not act (a docker failure, an enabled
// assignment with no registration token). A refusal is the leader's to make and
// arrives as `enabled=false`, so this function has no refusals of its own.
func (a *Agent) Apply(ctx context.Context, as Assignment) (Result, error) {
	var res Result
	name := RunnerName(a.Node)
	rec := AgentRecord{
		Node:        a.Node,
		RunnerName:  name,
		Enabled:     as.Enabled,
		Org:         as.Org,
		Group:       as.Group,
		SocketPath:  as.SocketPath,
		SocketProxy: as.SocketProxy,
		Memory:      as.Memory,
		AppliedAt:   a.now().Unix(),
	}
	if as.Refused {
		rec.Refused, rec.RefusedReason = true, as.Reason
	}
	if prev, ok := a.Record(); ok {
		rec.RegisteredAt = prev.RegisteredAt
	}

	local, exists, err := a.inspect(ctx, name)
	if err != nil {
		rec.Container = "unknown"
		rec.LastError = err.Error()
		a.save(rec)
		return res, err
	}

	// 0. Held by an operator: the leader recorded a kill from the console and
	//    sent it here, because the leader can only remove containers on its own
	//    node. Remove ours and stay quiet until the hold is cleared by Restart —
	//    a held node that keeps recreating the runner makes the button a lie.
	if as.Held {
		// StopRunner runs even when the runner container is already gone: "the runner
		// is absent" says nothing about its dind container, and a job that crashed
		// leaves a privileged daemon behind. A Kill button that leaves that running
		// is a button that did not do what it says.
		if err := StopRunner(ctx, a.Docker, name); err != nil {
			rec.Container, rec.LastError = "present", err.Error()
			a.save(rec)
			return res, fmt.Errorf("remove held runner container %s: %w", name, err)
		}
		if exists {
			res.Removed = append(res.Removed, name)
			a.Log.Info().Str("runner", name).Str("reason", as.Reason).
				Msg("runners — removed this node's runner container (held by an operator)")
		}
		rec.Container = "held"
		rec.RegisteredAt = 0
		rec.LastError = ""
		a.save(rec)
		return res, nil
	}

	// 1b. Refused on arithmetic: this node cannot hold the configured cap. Said
	//    at WARN with the reason the leader sent (both numbers), because the fix
	//    is an operator's and a silent no-runner looks like a broken feature.
	if as.Refused {
		if exists {
			if err := StopRunner(ctx, a.Docker, name); err != nil {
				rec.Container, rec.LastError = "present", err.Error()
				a.save(rec)
				return res, fmt.Errorf("remove refused runner container %s: %w", name, err)
			}
			res.Removed = append(res.Removed, name)
		}
		rec.Container = "refused"
		rec.RegisteredAt = 0
		rec.LastError = ""
		a.save(rec)
		a.Log.Warn().Str("runner", name).Str("reason", as.Reason).
			Msg("runners — NOT starting this node's runner: the configured memory cap does not fit this node")
		return res, nil
	}

	// 1. Disabled (or not selected): the container comes off this node. The
	//    registration is the leader's to remove — it told this node to stop, so
	//    it knows the name is no longer wanted.
	if !as.Enabled {
		if exists {
			if err := StopRunner(ctx, a.Docker, name); err != nil {
				rec.Container, rec.LastError = "present", err.Error()
				a.save(rec)
				return res, fmt.Errorf("remove runner container %s: %w", name, err)
			}
			res.Removed = append(res.Removed, name)
			a.Log.Info().Str("runner", name).Str("reason", as.Reason).
				Msg("runners — removed this node's runner container (assignment disabled)")
		}
		rec.Container = "absent"
		rec.RegisteredAt = 0
		a.save(rec)
		return res, nil
	}

	if !as.SocketProxy {
		// The fallback is legal and documented; it must be loud, because the
		// container now holds root on this host.
		a.Log.Warn().Str("runner", name).
			Msg("runners — assignment asks for the RAW docker socket: the container has root-equivalent access to this host")
	}

	// 2. Enabled and already running: nothing to do. The container registered
	//    itself with the token it was created with.
	if exists && local.Running() {
		rec.Container = "running"
		a.save(rec)
		return res, nil
	}

	// 3. A container that exists but is not running is either still starting or
	//    spent (an ephemeral runner exits after one job). A fresh one is left
	//    alone for the cooldown; a spent one is replaced.
	if exists {
		if rec.RegisteredAt > 0 && a.now().Sub(time.Unix(rec.RegisteredAt, 0)) < a.cooldown() {
			rec.Container = "starting"
			a.save(rec)
			a.Log.Debug().Str("runner", name).Dur("cooldown", a.cooldown()).
				Msg("runners — container is not running yet and was registered recently; leaving it alone")
			return res, nil
		}
		if err := StopRunner(ctx, a.Docker, name); err != nil {
			rec.Container, rec.LastError = "exited", err.Error()
			a.save(rec)
			return res, fmt.Errorf("remove the finished runner container %s: %w", name, err)
		}
	}

	if as.RegistrationToken == "" {
		// An enabled answer without a token means the leader could not mint
		// one; starting the container would fail with a GitHub 401 inside the
		// runner and look like a workflow problem.
		err := fmt.Errorf("assignment for org %s is enabled but carries no registration token", as.Org)
		rec.Container, rec.LastError = "absent", err.Error()
		a.save(rec)
		return res, err
	}

	// Persist the node-level part of the decision before the container work: the
	// watchdog reads it, and it must reflect the leader's answer even when starting
	// the runner fails two lines below.
	if a.StatePath != "" {
		if err := WriteNodePolicy(filepath.Dir(a.StatePath), NodePolicy{DockerRestart: as.NodeDockerRestart}); err != nil {
			a.Log.Warn().Err(err).Msg("runners — could not persist the node policy; the watchdog keeps its previous setting")
		}
	}

	spec := Spec{
		Org:         as.Org,
		Group:       as.Group,
		RunnerName:  name,
		Image:       DefaultImage,
		Memory:      as.Memory,
		Token:       as.RegistrationToken,
		SocketProxy: as.SocketProxy,
		SocketPath:  as.SocketPath,
		DinD:        as.DinD,
	}
	if _, err := spec.Argv(); err != nil {
		rec.Container, rec.LastError = "absent", err.Error()
		a.save(rec)
		return res, fmt.Errorf("render the runner for org %s: %w", as.Org, err)
	}
	if err := StartRunner(ctx, a.Docker, spec); err != nil {
		rec.Container, rec.LastError = "absent", err.Error()
		a.save(rec)
		return res, fmt.Errorf("start the runner container for org %s: %w", as.Org, err)
	}

	rec.Container = "running"
	rec.RegisteredAt = a.now().Unix()
	a.save(rec)
	res.Registered = append(res.Registered, as.Org)
	mount := "socket proxy"
	switch {
	case as.DinD:
		mount = "dind (private daemon, removed with the job)"
	case !as.SocketProxy:
		mount = "raw docker socket (debug fallback)"
	}
	a.Log.Info().Str("org", as.Org).Str("runner", name).Str("group", as.Group).
		Str("memory", as.Memory).Str("docker_access", mount).
		Msg("runners — started this node's runner from the leader's assignment")
	return res, nil
}

func (a *Agent) inspect(ctx context.Context, name string) (Container, bool, error) {
	if a.Docker == nil {
		return Container{}, false, nil
	}
	c, ok, err := a.Docker.Inspect(ctx, name)
	if err != nil {
		return Container{}, false, fmt.Errorf("inspect runner container %s: %w", name, err)
	}
	return c, ok, nil
}
