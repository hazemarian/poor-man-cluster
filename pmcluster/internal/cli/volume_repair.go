package cli

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/manifest"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// localVolumeRoot is the effective storage root for this node: the volume_root
// setting when it holds a value, otherwise the default. An empty row (the
// setting can exist but be blank) counts as unset — otherwise the directory
// work below would be silently skipped.
func localVolumeRoot(ctx context.Context, st *store.Store) string {
	if st != nil {
		if v := st.SettingDefault(ctx, cluster.SettingVolumeRoot(), ""); v != "" {
			return v
		}
	}
	return manifest.DefaultVolumeRoot
}

// volumeRepairInterval is how often each daemon re-checks the volume
// directories it owns. A pass is cheap: one service list plus one inspect per
// service pinned to this node.
const volumeRepairInterval = 30 * time.Second

// serviceForcer force-updates a single service (the dockerCLIDeployer in the
// daemon satisfies it). Kept narrow so the repair is easy to test.
type serviceForcer interface {
	ForceUpdateService(ctx context.Context, fullName string) error
}

// imageUserResolver is the optional capability a runtime client can offer to
// answer "what uid does this image run as". Kept out of runtime.Client because
// the repair treats it as a best effort: a backend that cannot answer simply
// leaves ownership alone instead of failing the pass.
type imageUserResolver interface {
	ImageUserIDs(ctx context.Context, ref string) (uid, gid int, ok bool, err error)
}

// ownershipSweepInterval is how often a directory whose top level is already
// owned correctly is walked for entries that are not. A restore can write
// root-owned files into an existing, correctly-owned directory — the top-level
// check cannot see that, and walking every volume on every 30s tick would be
// wasteful on real data.
const ownershipSweepInterval = 10 * time.Minute

// repairState is the per-daemon memory of the repair loop: resolved image users
// (so a named USER costs one container create, not one per pass) and when each
// path was last swept.
type repairState struct {
	mu    sync.Mutex
	users map[string]userEntry
	swept map[string]time.Time
}

type userEntry struct {
	uid, gid int
	ok       bool
	at       time.Time
}

// userCacheTTL bounds both answers: an image can be updated to a different USER,
// and an image that was missing at boot is usually pulled by the first deploy.
const userCacheTTL = 10 * time.Minute

func newRepairState() *repairState {
	return &repairState{users: map[string]userEntry{}, swept: map[string]time.Time{}}
}

// owner returns the uid/gid an image runs as, cached.
func (s *repairState) owner(ctx context.Context, res imageUserResolver, ref string) (int, int, bool) {
	if res == nil || ref == "" {
		return 0, 0, false
	}
	s.mu.Lock()
	if e, ok := s.users[ref]; ok && time.Since(e.at) < userCacheTTL {
		s.mu.Unlock()
		return e.uid, e.gid, e.ok
	}
	s.mu.Unlock()
	uid, gid, ok, err := res.ImageUserIDs(ctx, ref)
	if err != nil {
		return 0, 0, false
	}
	s.mu.Lock()
	if len(s.users) > 256 {
		s.users = map[string]userEntry{}
	}
	s.users[ref] = userEntry{uid: uid, gid: gid, ok: ok, at: time.Now()}
	s.mu.Unlock()
	return uid, gid, ok
}

// sweepDue reports whether path should be walked for ownership now.
func (s *repairState) sweepDue(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.swept) > 512 {
		s.swept = map[string]time.Time{}
	}
	last, ok := s.swept[path]
	if ok && time.Since(last) < ownershipSweepInterval {
		return false
	}
	s.swept[path] = time.Now()
	return true
}

// startVolumeRepairLoop keeps the volume directories of the stateful services
// the Swarm placed on THIS node present on disk. The deploying host is not
// necessarily the node a stack is pinned to (stateful placement round-robins
// over the storage nodes), and the Swarm refuses to start a task whose bind
// source does not exist — `failed to populate volume: mount
// <volume_root>/<app>/<name>: no such file or directory`. Creating the
// directories is inherently node-local, so every daemon does its own, leader
// or not.
func startVolumeRepairLoop(ctx context.Context, dc runtime.Client, forcer serviceForcer, volumeRoot func() string, log zerolog.Logger) {
	go func() {
		tick := time.NewTicker(volumeRepairInterval)
		defer tick.Stop()
		state := newRepairState()
		repair := func() {
			root := volumeRoot()
			created, updated, owned, err := repairLocalVolumeDirs(ctx, dc, forcer, root, state, log)
			if err != nil {
				log.Warn().Err(err).Msg("volume repair")
				return
			}
			if created > 0 {
				log.Info().Int("directories", created).Int("services", updated).
					Str("volume_root", root).Msg("volume repair created missing directories")
			}
			if owned > 0 {
				log.Info().Int("entries", owned).
					Str("volume_root", root).Msg("volume repair restored app-data ownership")
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
func repairLocalVolumeDirs(ctx context.Context, dc runtime.Client, forcer serviceForcer, root string, state *repairState, log zerolog.Logger) (created, updated, owned int, err error) {
	if dc == nil {
		return 0, 0, 0, nil
	}
	if state == nil {
		state = newRepairState()
	}
	resolver, _ := dc.(imageUserResolver)
	host, herr := os.Hostname()
	if herr != nil {
		return 0, 0, 0, fmt.Errorf("hostname: %w", herr)
	}
	svcs, err := dc.ServiceList(ctx)
	if err != nil {
		// A worker cannot list services; the repair is a manager-side nicety,
		// so stay quiet and try again on the next tick.
		if strings.Contains(err.Error(), "not a swarm manager") {
			return 0, 0, 0, nil
		}
		return 0, 0, 0, fmt.Errorf("list services: %w", err)
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
				// A raw host bind is only touched inside the managed root.
				if !underVolumeRoot(root, m.Source) {
					continue
				}
				path = m.Source
			case "volume":
				// A stack volume is a local-driver bind: the mount's source is
				// just the volume name, and the host directory lives on the
				// volume. pmcluster renders these itself, so the device is
				// inside the managed layout by construction and needs no root
				// cross-check (this is what makes the repair work on a standby
				// manager, which has no local store to read the root from).
				vol, verr := dc.VolumeInspect(ctx, m.Source)
				if verr != nil || !vol.Bind {
					continue
				}
				path = vol.Device
			default:
				continue
			}
			if path == "" {
				continue
			}
			if _, serr := os.Stat(path); serr != nil {
				// A missing directory on a node that has run this service before
				// is not a fresh start — it is data that is gone. Creating the
				// directory would let the app boot on an empty database and serve
				// it as if nothing happened; failing the task loudly is the
				// honest outcome. The operator creates the directory themselves
				// when the emptiness is intended.
				if ran, rerr := serviceRanBefore(ctx, dc, svc.ID); rerr == nil && ran {
					log.Error().Str("service", svc.Name).Str("path", path).
						Msg("volume repair: refusing to recreate a data directory that has disappeared — this node has run this service before, so the data lived here; create the directory yourself if its absence is intended")
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
			// Existence is not enough: the directory must be writable by the user
			// the image runs as. Adoption copies this tree as root and a restore
			// writes into it as root, and an app that cannot write its own
			// database exits on startup — every five seconds, tearing down and
			// rebuilding its network sandbox each time.
			n, oerr := enforceOwnership(ctx, resolver, state, ins.Image, path)
			if oerr != nil {
				log.Warn().Err(oerr).Str("service", svc.Name).Str("path", path).
					Msg("volume repair: ownership")
				continue
			}
			owned += n
			if n > 0 {
				log.Info().Str("service", svc.Name).Str("path", path).Int("entries", n).
					Msg("volume repair: restored app-data ownership")
			}
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
	return created, updated, owned, nil
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

// enforceOwnership makes path and everything under it owned by the image's user.
// Cheap when nothing is wrong: one stat, and the image resolution comes from the
// per-daemon cache. The full walk happens when the top level is owned by someone
// else (the adoption/restore signature) or on the slow sweep that catches
// root-owned files inside an otherwise correct directory.
func enforceOwnership(ctx context.Context, res imageUserResolver, state *repairState, imageRef, path string) (int, error) {
	wantUID, wantGID, ok := state.owner(ctx, res, imageRef)
	if !ok {
		return 0, nil
	}
	haveUID, _, err := pathOwner(path)
	if err != nil {
		return 0, err
	}
	if haveUID == wantUID && !state.sweepDue(path) {
		return 0, nil
	}
	return chownTree(path, wantUID, wantGID)
}

// pathOwner reads a path's uid/gid without following a symlink.
func pathOwner(path string) (uid, gid int, err error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("no ownership for %s on this platform", path)
	}
	return int(st.Uid), int(st.Gid), nil
}

// chownTree sets uid/gid on every entry under path whose owner differs, and
// returns how many it changed. Symlinks are changed with Lchown so their targets
// (which may live outside the volume) are untouched. An unreadable subtree is
// skipped rather than failing the whole pass: a partial repair still makes the
// app writable at the top.
func chownTree(path string, uid, gid int) (int, error) {
	changed := 0
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil // unreadable subtree: leave it, keep walking siblings
		}
		haveUID, haveGID, oerr := pathOwner(p)
		if oerr != nil {
			return nil
		}
		if haveUID == uid && haveGID == gid {
			return nil
		}
		if cerr := os.Lchown(p, uid, gid); cerr != nil {
			if p == path {
				return cerr // the top level matters; deeper failures do not
			}
			return nil
		}
		changed++
		return nil
	})
	return changed, err
}

// serviceRanBefore reports whether this service has ever had a task that
// started on any node. A rejected task never ran (that is the "the bind source
// does not exist" case, which is what the repair exists to fix); shutdown,
// complete and failed all mean a container was alive at some point.
func serviceRanBefore(ctx context.Context, dc runtime.Client, serviceID string) (bool, error) {
	tasks, err := dc.ServiceTasks(ctx, serviceID)
	if err != nil {
		return false, err
	}
	for _, t := range tasks {
		switch strings.ToLower(t.State) {
		case "shutdown", "complete", "failed", "running":
			return true, nil
		}
	}
	return false, nil
}
