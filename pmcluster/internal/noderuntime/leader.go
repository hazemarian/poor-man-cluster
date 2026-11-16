package noderuntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/controlplane"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// leaderPollInterval is how often a standby daemon re-checks whether this node
// has become the Docker Swarm leader.
const leaderPollInterval = 15 * time.Second

// WaitForSwarmLeadership blocks until the local Docker node is the current
// Swarm leader (or the context is cancelled). Non-leader managers run as
// standby: the daemon does NOT serve — it polls until Swarm elects this node
// leader (failover), then the caller proceeds to open the store and serve.
// It returns immediately when Docker is unavailable or the node is not a
// swarm member (standalone/local mode) — only a confirmed non-leader blocks.
func WaitForSwarmLeadership(ctx context.Context, dc runtime.Client, log zerolog.Logger) error {
	if dc == nil {
		return nil
	}
	host, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("get hostname for leader check: %w", err)
	}
	t := time.NewTicker(leaderPollInterval)
	defer t.Stop()
	workerLogged := false
	for {
		leader, found, err := IsSwarmLeader(ctx, dc, host)
		if err != nil && IsSwarmWorker(ctx, dc) {
			// A genuine swarm WORKER (swarm active, ControlAvailable false).
			// `docker node ls` fails with "this node is not a swarm manager",
			// but unlike a standalone node it must NOT serve and start a
			// control loop that errors every pass — stand by silently
			// instead, re-checking so a later promotion to manager resumes.
			if !workerLogged {
				log.Info().Str("node", host).Msg("this node is a swarm worker — standing by (no control loop)")
				workerLogged = true
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
			}
			continue
		}
		switch {
		case err != nil:
			// Docker unreachable: can't confirm swarm membership — treat as
			// standalone/local and serve immediately.
			log.Warn().Err(err).Msg("leader check unavailable; serving (standalone/local mode)")
			return nil
		case !found:
			log.Warn().Str("node", host).Msg("node not in swarm node list; serving (standalone/local mode)")
			return nil
		case leader:
			return nil
		default:
			log.Info().Str("node", host).Msg("not the swarm leader — standing by (failover standby)")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// IsSwarmWorker reports whether the local Docker daemon is a Swarm WORKER: a
// member of an ACTIVE swarm that lacks manager (control) access. Such a node
// can never run the control loop — every manager-only call (node/service/
// config list) fails with "this node is not a swarm manager" — so the daemon
// must stand by silently instead of serving and starting a reconcile loop
// that errors every pass. A standalone node (swarm inactive) or a manager
// returns false, preserving the existing behaviour.
func IsSwarmWorker(ctx context.Context, dc runtime.Client) bool {
	if dc == nil {
		return false
	}
	info, err := dc.Info(ctx)
	if err != nil {
		// Docker unreachable: can't confirm worker status — treat as
		// standalone/local (existing behaviour).
		return false
	}
	return info.SwarmLocalNodeState == "active" && !info.SwarmControlAvailable
}

// IsSwarmLeader reports whether the node with the given hostname is the
// current Swarm leader, per the Raft-elected leader flag on the node list.
// found is false when the hostname is not present in the node list.
func IsSwarmLeader(ctx context.Context, dc runtime.Client, host string) (leader, found bool, err error) {
	// A nil client is a caller error, not a leaderless node, and it must not be a
	// nil-pointer panic: this is called from a background probe, where a panic takes
	// the whole daemon with it and the only visible symptom is /health never
	// answering. Answer the question with an error the caller can log instead.
	if dc == nil {
		return false, false, errors.New("no docker client")
	}
	nodes, err := dc.NodeList(ctx)
	if err != nil {
		return false, false, err
	}
	for _, n := range nodes {
		if n.Hostname == host {
			return n.IsLeader, true, nil
		}
	}
	return false, false, nil
}

// WatchSwarmLeadership streams the node's leadership state: true when this
// node becomes Swarm leader, false when it loses leadership. It emits an
// initial value immediately (leader or not) so callers can start/stop the
// leader-only control loop without waiting a full poll. The channel is
// closed when the context is cancelled or Docker becomes unreachable.
func WatchSwarmLeadership(ctx context.Context, dc runtime.Client, log zerolog.Logger) <-chan bool {
	out := make(chan bool)
	go func() {
		defer close(out)
		if dc == nil {
			// Standalone/local mode: always "leader" of itself.
			select {
			case out <- true:
			case <-ctx.Done():
			}
			return
		}
		host, err := os.Hostname()
		if err != nil {
			log.Error().Err(err).Msg("get hostname for leader watch")
			return
		}
		t := time.NewTicker(leaderPollInterval)
		defer t.Stop()
		current := false
		first := true
		workerLogged := false
		for {
			leader, found, err := IsSwarmLeader(ctx, dc, host)
			switch {
			case err != nil && IsSwarmWorker(ctx, dc):
				// A genuine swarm WORKER. Stay idle — never emit leader-true,
				// so the caller never starts the reconcile loop — logging the
				// standby message once, and re-checking each poll so a later
				// promotion to manager starts the loop.
				if !workerLogged {
					log.Info().Str("node", host).Msg("this node is a swarm worker — standing by (no control loop)")
					workerLogged = true
				}
				current, first = false, false
			case err != nil || !found:
				// Docker unreachable / not a swarm member: standalone mode.
				if current || first {
					select {
					case out <- true:
					case <-ctx.Done():
						return
					}
				}
				current, first = true, false
			case leader:
				if !current || first {
					log.Info().Str("node", host).Msg("became swarm leader — control loop active")
					select {
					case out <- true:
					case <-ctx.Done():
						return
					}
				}
				// current=true means "we already emitted leader-true for this
				// state"; a later poll that is still leader must stay silent so
				// the caller doesn't cancel + restart its loops every poll.
				current, first = true, false
			default:
				if current || first {
					select {
					case out <- false:
					case <-ctx.Done():
						return
					}
				}
				current, first = false, false
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return out
}

// AdoptOnLeadershipGain decides whether this node may act as the cluster's writer after
// a follower→leader transition, and says so loudly either way. It returns ready: false
// only when there is NOTHING TO SERVE — a node that has never applied a snapshot and whose
// store holds no cluster state. A STALE mirror is served: the operator's decision is that a
// promotion on a few-minutes-old snapshot beats no leader, so the age and version are
// logged as the adoption happens rather than turned into a refusal.
//
// Nothing is copied, renamed or swapped. The node has been mirroring the cluster's state
// the whole time it was a follower, so on gaining leadership it keeps serving the store it
// already has and becomes the writer (ADR 0005). The old path closed the SQLite handle,
// renamed a staged data.db and .encryption_key into place and reopened; that existed
// because the mirror was a projection, and it is gone with the projection.
//
// mirrored reports whether this process ran the mirror before the transition — it is
// carried into the log rather than used to decide, because the decision is about the STORE,
// not about which loop this process ran.
//
// Failure is contained: the refusal is logged with the reason, the store stays open and
// keeps answering reads with the mirror's age attached, and the next leadership transition
// checks again.
func AdoptOnLeadershipGain(ctx context.Context, kit *controlplane.Kit, st *store.Store, gate *AdoptionGate, mirrored bool, log zerolog.Logger) bool {
	if kit == nil {
		return true // no control plane to check (standalone); serve as before
	}
	end := gate.begin()
	adoption, err := func() (controlplane.Adoption, error) {
		// defer, so the window closes even if the check panics — otherwise the node
		// would answer 503 for the rest of its life.
		defer end()
		return kit.AdoptOnLeadership(ctx, st, mirrored)
	}()
	if err != nil {
		log.Error().Err(err).Bool("mirrored", mirrored).
			Msg("refusing leadership: this node has no cluster state to serve — the control loop is NOT starting (reads keep answering with the mirror's age; the next leadership transition retries)")
		return false
	}
	if !adoption.Ready {
		return false
	}
	log.Info().
		Str("reason", adoption.Reason).
		Str("mirrored_leader", adoption.Leader).
		Int64("mirrored_version", adoption.Version).
		Dur("mirror_age", adoption.Age).
		Bool("source", adoption.Source).
		Bool("key_ok", adoption.KeyOK).
		Msg("serving the mirrored store; this node is the cluster's writer")
	return true
}

// ConfirmMirrorAtStartup says what this node's store is seeded with, at boot, before any
// handle is opened for serving. It replaces the old startup control-plane restore: the
// store is seeded by the MIRROR, so the only thing to do here is state what this node
// holds — never to copy files into it (ADR 0005).
//
// A manager with no store yet is not an error. It starts, serves /health, and waits for
// the first snapshot; saying that plainly is the job, because a node that answers with
// empty tables and no explanation is indistinguishable from one that lost its data.
func ConfirmMirrorAtStartup(ctx context.Context, cfg *config.Config, log zerolog.Logger) {
	if cfg == nil {
		return
	}
	if _, err := os.Stat(cfg.DBPath()); err != nil {
		log.Info().Str("path", cfg.DBPath()).
			Msg("no cluster state on this node yet: the store is seeded by the mirror — waiting for the first snapshot (nothing is copied onto this node any more)")
		return
	}
	st, err := store.OpenExisting(cfg.DBPath())
	if err != nil {
		log.Warn().Err(err).Msg("cannot read this node's cluster state at startup; it will be reported once the store opens")
		return
	}
	defer func() { _ = st.Close() }()

	state, err := st.SyncState(ctx)
	switch {
	case errors.Is(err, store.ErrNeverSynced):
		log.Info().Msg("this node has never applied a snapshot — waiting for the first one from the leader")
	case err != nil:
		log.Warn().Err(err).Msg("cannot read this node's mirror state at startup")
	default:
		// Same rule as every other age on this platform: a signal that keeps moving while
		// the thing is healthy (checked_at moves on a 304 too), never when content last
		// changed, and never a negative age from a clock that stepped backwards.
		age := time.Since(time.Unix(state.CheckedAt, 0))
		if age < 0 {
			age = 0
		}
		log.Info().Str("leader", state.Leader).Int64("version", state.Version).
			Dur("age", age.Truncate(time.Second)).
			Msg("cluster state is mirrored from the leader")
	}
}
