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
	for {
		leader, found, err := isSwarmLeader(ctx, dc, host)
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
		for {
			leader, found, err := isSwarmLeader(ctx, dc, host)
			switch {
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
		// (BUG-020, seen live in TC10-A).
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
