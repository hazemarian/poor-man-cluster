package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/controlplane"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// leaderPollInterval is how often a standby daemon re-checks whether this node
// has become the Docker Swarm leader.
const leaderPollInterval = 15 * time.Second

// controlPlaneArchiveDir is where the control-plane backup agent writes its
// pmcluster-ctlplane-*.tar.gz archives. Overridable in tests.
var controlPlaneArchiveDir = backups.DefaultArchiveDir

// waitForSwarmLeadership blocks until the local Docker node is the current
// Swarm leader (or the context is cancelled). Non-leader managers run as
// standby: the daemon does NOT serve — it polls until Swarm elects this node
// leader (failover), then the caller proceeds to open the store and serve.
// It returns immediately when Docker is unavailable or the node is not a
// swarm member (standalone/local mode) — only a confirmed non-leader blocks.
func waitForSwarmLeadership(ctx context.Context, dc runtime.Client, log zerolog.Logger) error {
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
		leader, found, err := isSwarmLeader(ctx, dc, host)
		if err != nil && isSwarmWorker(ctx, dc) {
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

// isSwarmWorker reports whether the local Docker daemon is a Swarm WORKER: a
// member of an ACTIVE swarm that lacks manager (control) access. Such a node
// can never run the control loop — every manager-only call (node/service/
// config list) fails with "this node is not a swarm manager" — so the daemon
// must stand by silently instead of serving and starting a reconcile loop
// that errors every pass. A standalone node (swarm inactive) or a manager
// returns false, preserving the existing behaviour.
func isSwarmWorker(ctx context.Context, dc runtime.Client) bool {
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

// isSwarmLeader reports whether the node with the given hostname is the
// current Swarm leader, per the Raft-elected leader flag on the node list.
// found is false when the hostname is not present in the node list.
func isSwarmLeader(ctx context.Context, dc runtime.Client, host string) (leader, found bool, err error) {
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
			leader, found, err := isSwarmLeader(ctx, dc, host)
			switch {
			case err != nil && isSwarmWorker(ctx, dc):
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

// leaderGainSteps runs the two steps of a follower→leader transition in the
// order that matters: a fresh registry credential first, then the reconcile
// loop whose deploys freeze that credential into service specs.
//
// Extracted so the order is a tested fact rather than a comment: a test passes
// recording fakes and asserts the mint ran exactly once, before the reconcile.
func leaderGainSteps(ctx context.Context, mint func(context.Context), reconcile func()) {
	mint(ctx)
	reconcile()
}

// adoptOnLeadershipGain runs the control-plane freshness predicate once for a
// follower→leader transition and adopts the newest Raft-replicated kit when this
// node is behind it.
//
// It is the mid-run twin of the startup check in serve (ensureControlPlaneFresh):
// the same predicate — no key, a missing or older `.state_revision`, or a
// different encryption key — but the store is already open and serving, so
// adoption closes the SQLite handle, renames the staged data.db and
// .encryption_key into place, and reopens (controlplane.StoreSwapper). The
// encryption key is adopted with the DB, so the in-memory Cipher is reloaded
// when the key changed; otherwise every ciphertext in the freshly adopted
// database would be undecryptable.
//
// Failure is contained: the Kit logs the reason and leaves the current database
// in place, and this function keeps serving it. Exiting here would turn a stale
// leader into no leader at all.
func adoptOnLeadershipGain(ctx context.Context, kit *controlplane.Kit, swap controlplane.StoreSwapper, cipher *credentials.Cipher, cfg *config.Config, gate *adoptionGate, log zerolog.Logger) {
	if kit == nil {
		return
	}
	end := gate.begin()
	adoption, err := func() (controlplane.Adoption, error) {
		// defer, so the window closes even if the adoption panics — otherwise
		// the node would answer 503 for the rest of its life.
		defer end()
		return kit.AdoptOnLeadership(ctx, swap)
	}()

	switch {
	case err != nil:
		log.Error().Err(err).Msg("control-plane adoption on leadership gain did not happen; continuing to serve the CURRENT database")
		return
	case !adoption.Adopted:
		return
	}

	log.Info().
		Str("config", adoption.Config).
		Str("revision", adoption.Revision.Format(time.RFC3339Nano)).
		Str("reason", adoption.Reason).
		Bool("key-adopted", adoption.KeyChanged).
		Msg("control plane adopted on gaining leadership")
	if !adoption.KeyChanged {
		return
	}
	if cipher == nil {
		log.Error().Msg("the adopted control plane brought a different encryption key but no cipher is loaded; credentials stay unreadable until the daemon restarts")
		return
	}
	if err := cipher.Reload(cfg.EncryptionKeyPath()); err != nil {
		log.Error().Err(err).Msg("could not load the encryption key adopted with the control plane; credentials stay unreadable until the daemon restarts")
		return
	}
	log.Info().Msg("reloaded the encryption key adopted with the control plane")
}

// ensureControlPlaneFresh restores the control plane on promotion. Two
// sources, newest-first:
//
//  1. The Raft-replicated survivor kit (L2): the leader snapshots the data
//     kit + encryption key into pmcluster_state_* Docker configs, replicated
//     to every manager by Swarm itself — no host-to-host archive shipping.
//     Restored when the local data.db is missing or older than the snapshot.
//  2. The tarball archive fallback: pre-L2 clusters that only have
//     pmcluster-ctlplane-*.tar.gz archives on /var/stack/backup.
//
// When the local DB is already at least as fresh as the newest snapshot (e.g.
// shared/replicated storage such as LINSTOR keeps the data dir current on
// every manager), no restore happens — restoring would clobber a live,
// current database. Returns (restored bool, err).
func ensureControlPlaneFresh(ctx context.Context, cfg *config.Config, dc runtime.Client, log zerolog.Logger) (bool, error) {
	kit := &controlplane.Kit{Docker: dc, DataDir: cfg.DataDir, Log: log}
	restored, err := kit.Restore(ctx)
	if err == nil {
		return restored, nil // Raft config path handled it (restored, or DB current)
	}
	if !errors.Is(err, controlplane.ErrNoSnapshots) && !controlplane.IsSwarmUnavailable(err) {
		return false, fmt.Errorf("control-plane Raft restore: %w", err)
	}
	if controlplane.IsSwarmUnavailable(err) {
		// The swarm has no Raft leader right now (quorum lost), so the
		// replicated kit is unreadable. That is transient, not a
		// control-plane failure — never fatal: fall through to the local
		// tarball archive and, if there is none, keep serving with the local
		// DB. Safe because the reconcile loop only starts once leadership is
		// confirmed, so there is nothing to reconcile during the outage.
		// Without this the daemon exits and systemd restart-loops it
		// (seen live during a whole quorum outage).
		log.Warn().Err(err).Msg("swarm unavailable; deferring control-plane restore")
	}

	// No state configs yet (pre-L2 cluster or standalone): fall back to the
	// tarball archive path.
	newest, err := backups.NewestControlPlaneArchive(controlPlaneArchiveDir)
	if err != nil {
		return false, fmt.Errorf("scan control-plane archives: %w", err)
	}
	if newest == "" {
		log.Info().Msg("no control-plane archive found; keeping local data")
		return false, nil
	}
	archInfo, err := os.Stat(newest)
	if err != nil {
		return false, fmt.Errorf("stat newest control-plane archive %s: %w", newest, err)
	}
	dbInfo, err := os.Stat(cfg.DBPath())
	switch {
	case err == nil:
		// DB exists: restore only when it is OLDER than the newest archive.
		// A DB at least as fresh as the archive is already current — the
		// shared-storage (LINSTOR) case — and must not be overwritten.
		if !dbInfo.ModTime().Before(archInfo.ModTime()) {
			log.Info().Str("archive", newest).Msg("local control-plane DB is current (shared storage?); no restore needed")
			return false, nil
		}
		log.Info().Str("archive", newest).Msg("local control-plane DB is older than the newest archive; restoring")
	case !os.IsNotExist(err):
		return false, fmt.Errorf("stat local data.db: %w", err)
	default:
		log.Info().Str("archive", newest).Msg("no local control-plane DB; restoring from newest archive")
	}

	n, err := backups.RestoreControlPlane(newest, cfg.DataDir)
	if err != nil {
		return false, fmt.Errorf("restore control plane from %s: %w", newest, err)
	}
	log.Info().Int("files", n).Str("archive", newest).Msg("control-plane data restored")
	return true, nil
}
