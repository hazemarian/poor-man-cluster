package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// volumeRepairInterval is how often each daemon re-checks the volume
// directories it owns. A pass is cheap: one service list plus one inspect per
// service pinned to this node.
const volumeRepairInterval = 30 * time.Second

// serviceForcer force-updates a single service (the dockerCLIDeployer in the
// daemon satisfies it). Kept narrow so the repair is easy to test.
type serviceForcer interface {
	ForceUpdateService(ctx context.Context, fullName string) error
}

// startVolumeRepairLoop keeps the volume directories of the stateful services
// the Swarm placed on THIS node present on disk. The deploying host is not
// necessarily the node a stack is pinned to (stateful placement round-robins
// over the storage nodes), and the Swarm refuses to start a task whose bind
// source does not exist — `failed to populate volume: mount
// <volume_root>/<app>/<name>: no such file or directory` (BUG-026). Creating
// the directories is inherently node-local, so every daemon does its own,
// leader or not.
func startVolumeRepairLoop(ctx context.Context, dc runtime.Client, forcer serviceForcer, volumeRoot func() string, log zerolog.Logger) {
	go func() {
		tick := time.NewTicker(volumeRepairInterval)
		defer tick.Stop()
		repair := func() {
			root := volumeRoot()
			created, updated, err := repairLocalVolumeDirs(ctx, dc, forcer, root, log)
			if err != nil {
				log.Warn().Err(err).Msg("volume repair")
				return
			}
			if created > 0 {
				log.Info().Int("directories", created).Int("services", updated).
					Str("volume_root", root).Msg("volume repair created missing directories")
			}
		}
		repair()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				repair()
			}
		}
	}()
}

// repairLocalVolumeDirs makes sure every bind source under the volume root of a
// service pinned to this node exists, and force-updates the services it
// repaired so the Swarm retries their rejected tasks. Services without a node
// pin (role-based or global — the platform agents) are left alone: their binds
// are ensured by cluster up/update.
func repairLocalVolumeDirs(ctx context.Context, dc runtime.Client, forcer serviceForcer, root string, log zerolog.Logger) (created, updated int, err error) {
	if dc == nil {
		return 0, 0, nil
	}
	host, herr := os.Hostname()
	if herr != nil {
		return 0, 0, fmt.Errorf("hostname: %w", herr)
	}
	svcs, err := dc.ServiceList(ctx)
	if err != nil {
		// A worker cannot list services; the repair is a manager-side nicety,
		// so stay quiet and try again on the next tick.
		if strings.Contains(err.Error(), "not a swarm manager") {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("list services: %w", err)
	}
	for _, svc := range svcs {
		if svc.Node == "" || svc.Node != host {
			continue
		}
		ins, ierr := dc.ServiceInspect(ctx, svc.ID)
		if ierr != nil {
			log.Warn().Err(ierr).Str("service", svc.Name).Msg("volume repair: inspect service")
			continue
		}
		repaired := false
		for _, m := range ins.Mounts {
			var path string
			switch m.Type {
			case "bind":
				path = m.Source
			case "volume":
				// A stack volume is declared as a local-driver bind, so the
				// real host directory lives on the volume definition, not on
				// the mount (the mount's Source is just the volume name).
				vol, verr := dc.VolumeInspect(ctx, m.Source)
				if verr != nil {
					continue
				}
				path = vol.Device
			default:
				continue
			}
			if path == "" || !underVolumeRoot(root, path) {
				continue
			}
			if _, serr := os.Stat(path); serr == nil {
				continue
			}
			if merr := os.MkdirAll(path, 0o755); merr != nil {
				log.Warn().Err(merr).Str("service", svc.Name).Str("path", path).
					Msg("volume repair: create directory")
				continue
			}
			created++
			repaired = true
			log.Info().Str("service", svc.Name).Str("path", path).
				Msg("volume repair: created missing directory")
		}
		if !repaired || forcer == nil {
			continue
		}
		if ferr := forcer.ForceUpdateService(ctx, svc.ID); ferr != nil {
			log.Warn().Err(ferr).Str("service", svc.Name).Msg("volume repair: force update service")
			continue
		}
		updated++
	}
	return created, updated, nil
}

// underVolumeRoot reports whether p is the root itself or inside it. An empty
// root matches nothing, so the repair stays scoped to the managed layout.
func underVolumeRoot(root, p string) bool {
	if root == "" {
		return false
	}
	if p == root {
		return true
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
