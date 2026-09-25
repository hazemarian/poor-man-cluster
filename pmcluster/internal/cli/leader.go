package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
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

// ensureControlPlaneFresh restores the newest control-plane archive when the
// local data.db is missing or older than that archive. When the local DB is
// already at least as fresh as the newest archive (e.g. shared/replicated
// storage such as LINSTOR keeps the data dir current on every manager), no
// restore happens — restoring would clobber a live, current database.
// Returns (restored bool, err) — restored reports whether a backup was
// written.
func ensureControlPlaneFresh(ctx context.Context, cfg *config.Config, log zerolog.Logger) (bool, error) {
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
