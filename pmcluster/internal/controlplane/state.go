// Package controlplane owns the last piece of the control-plane's Docker-config
// channel: the encryption KEY config, and the leadership-gain check that reads it.
//
// What is left, and why. ADR 0005 retired the data kit — the mirror carries the whole
// control-plane document and adoption copies nothing, so no `data.db` ever needs to be
// replicated as a blob again — but ADR 0003's security split still stands: the cipher
// key never shares a blob with the ciphertext it unlocks. So this package publishes
// `pmcluster_state_key_<unixnano>` and nothing else, and compares a node's local key
// with the cluster's on a leadership gain.
//
// The join-time key route (`POST /internal/api/join/key`, served once, `409` after)
// replaces both, and then this package goes with them. Do not grow it back into a data
// channel: cluster state travels by the mTLS pull (internal/clustersnapshot).
package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// KeyNamePrefix names the config carrying only the encryption key. The family is still
// called `pmcluster_state_key` (not `…key`) so a node upgrading mid-flight finds the
// config it already knows; the data family it used to be paired with is gone.
const KeyNamePrefix = "pmcluster_state_key"

// Label keys and values carried on the key config.
const (
	// ManagedLabel marks pmcluster-managed configs (same value as
	// cluster.PmclusterLabel; kept local to avoid an import cycle).
	ManagedLabel = "io.pmcluster.managed"
	// LabelKind distinguishes config families.
	LabelKind = "pmcluster.kind"
	// KindKey marks the key-only config.
	KindKey = "state-key"
	// LabelSnapshotAt carries the RFC3339Nano publish time.
	LabelSnapshotAt = "pmcluster.snapshot-at"
	// LabelVersion mirrors the pmcluster.version label on the other managed
	// configs so the version-drift check can identify them.
	LabelVersion = "pmcluster.version"
)

const (
	// KeepKeyConfigs is how many key configs are retained. A node reading a name while a
	// prune races a publish must still find one.
	KeepKeyConfigs = 2
	// KeyPublishInterval is how often the leader re-checks the key for changes.
	KeyPublishInterval = 5 * time.Minute
)

// ErrNoKeyConfig is returned by newestKey when the cluster has never published one.
var ErrNoKeyConfig = errors.New("no control-plane key config exists")

// IsSwarmUnavailable reports whether err is a *transient Swarm availability*
// error rather than a genuine control-plane failure. Two cases qualify:
//
//   - this node is a worker ("This node is not a swarm manager") — swarm
//     configs are manager-only, so the replicated key is invisible here;
//   - the swarm is momentarily without a Raft leader ("does not have a
//     leader" / "too few managers are online") — quorum was lost, so even a
//     manager cannot read the config *right now*.
//
// Both mean the key config cannot be read at this instant, so callers degrade and retry
// later instead of failing fatally. That matters most when a daemon (re)starts during a
// quorum outage: without this it exits, systemd restarts it, and every manager crash-loops
// (seen live: a manager restarted 28 times while the swarm had no leader).
// The matched strings are stable Docker messages (daemon/cluster/errors.go
// errSwarmNotManager; SwarmKit raft errNoLeader).
func IsSwarmUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "is not a swarm manager") ||
		strings.Contains(msg, "does not have a leader") ||
		strings.Contains(msg, "too few managers are online") ||
		// A dead or wedged dockerd: the swarm is unreadable for exactly the same
		// reason a leaderless one is — this node cannot ask it anything. Treating it
		// as a control-plane failure made the daemon exit, so systemd restart-loop
		// it to `failed` on the node whose docker had just died, which is the one
		// node that had to stay up long enough for the docker watchdog to fire
		// (measured on nxt-sw-6-w, 2026-11-13).
		isDockerDaemonUnavailable(msg)
}

// isDockerDaemonUnavailable matches the client's own wording for "there is no
// daemon to talk to": an absent socket, a refused connection, or a daemon that
// accepted the call and never answered.
func isDockerDaemonUnavailable(msg string) bool {
	for _, s := range []string{
		"Cannot connect to the Docker daemon",
		"Is the docker daemon running",
		"connect: no such file or directory",
		"connect: connection refused",
		"context deadline exceeded",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// Kit publishes and reads the control-plane's KEY config, and answers the
// leadership-gain question about this node's own store (see adopt.go).
//
// It no longer publishes or restores the data kit: the mirror is the document and
// adoption copies nothing (ADR 0005). Create one per daemon; it holds no per-process
// state.
type Kit struct {
	Docker  runtime.Client
	DataDir string
	Log     zerolog.Logger
}

// DBPath returns the SQLite database path inside DataDir.
func (k *Kit) DBPath() string { return filepath.Join(k.DataDir, "data.db") }

// KeyPath returns the encryption key path inside DataDir.
func (k *Kit) KeyPath() string { return filepath.Join(k.DataDir, ".encryption_key") }

// PublishKey writes the local encryption key into a fresh `pmcluster_state_key_<ts>`
// config (Raft-replicated to every manager by Swarm itself), when its bytes differ from
// the newest one already published, then prunes older key configs.
//
// It is a no-op when Docker is unavailable, when this node has no key file, or when the
// published config already carries exactly these bytes. Comparing CONTENT rather than an
// mtime is deliberate: the key changes on a rotation and at no other time, and an mtime
// walk over the data dir was how the old kit re-published itself on every tick.
//
// Callers: the leader's periodic KeyLoop and the leadership-gain path in `serve`. Never
// called on a follower — the key config rides Raft, which only the leader writes.
func (k *Kit) PublishKey(ctx context.Context) error {
	if k.Docker == nil {
		return nil // standalone/local mode: nothing to replicate to
	}
	log := k.Log.With().Str("component", "controlplane").Logger()

	key, err := os.ReadFile(k.KeyPath())
	if err != nil {
		// No key on this node yet: nothing to publish, and inventing one here would
		// hand every manager a key the cluster's ciphertext was not written with.
		log.Debug().Err(err).Str("path", k.KeyPath()).
			Msg("no local encryption key to publish")
		return nil
	}

	existing, err := k.newest(ctx, KindKey)
	if err != nil {
		if IsSwarmUnavailable(err) {
			log.Debug().Err(err).Msg("swarm unavailable; skipping key config publish")
			return nil
		}
		return err
	}
	if existing != "" {
		inspect, err := k.Docker.ConfigInspect(ctx, existing)
		if err != nil {
			return fmt.Errorf("inspect newest key config %s: %w", existing, err)
		}
		if bytes.Equal(inspect.Data, key) {
			log.Debug().Str("config", existing).Msg("control-plane key unchanged; skipping publish")
			return nil
		}
	}

	now := time.Now()
	name := KeyNamePrefix + "_" + strconv.FormatInt(now.UnixNano(), 10)
	if err := k.Docker.ConfigCreate(ctx, runtime.ConfigSpec{
		Name: name,
		Data: key,
		Labels: map[string]string{
			ManagedLabel:    "true",
			LabelKind:       KindKey,
			LabelSnapshotAt: now.Format(time.RFC3339Nano),
			LabelVersion:    buildinfo.Version,
		},
	}); err != nil {
		return fmt.Errorf("create control-plane key config: %w", err)
	}
	log.Info().Str("config", name).Int("bytes", len(key)).
		Msg("control-plane encryption key published to a Raft-replicated config")

	if err := k.prune(ctx); err != nil {
		log.Warn().Err(err).Msg("prune control-plane key configs had issues")
	}
	return nil
}

// KeyLoop re-checks the key for changes on every tick and publishes when it moved, until
// ctx is cancelled. A no-op when Docker is unavailable.
func (k *Kit) KeyLoop(ctx context.Context, interval time.Duration) {
	if k.Docker == nil {
		return
	}
	if interval <= 0 {
		interval = KeyPublishInterval
	}
	log := k.Log.With().Str("component", "controlplane").Logger()
	t := time.NewTicker(interval)
	defer t.Stop()
	log.Debug().Dur("interval", interval).Msg("control-plane key publish loop armed (leader)")
	for {
		select {
		case <-ctx.Done():
			log.Debug().Msg("control-plane key publish loop stopped")
			return
		case <-t.C:
			if err := k.PublishKey(ctx); err != nil {
				log.Warn().Err(err).Msg("control-plane key publish had issues")
			}
		}
	}
}

// newestKey returns the newest KEY config published on this swarm: its name and the bytes
// it carries. ErrNoKeyConfig means the cluster has never published one.
//
// It reads the key family directly instead of deriving the key's name from a state config,
// which is what it used to do: a key config is now the only thing in the family, so it has
// to be findable on its own.
func (k *Kit) newestKey(ctx context.Context) (name string, key []byte, err error) {
	if k.Docker == nil {
		return "", nil, ErrNoKeyConfig // standalone/local mode: no Raft configs to read
	}
	name, err = k.newest(ctx, KindKey)
	if err != nil {
		return "", nil, err
	}
	if name == "" {
		return "", nil, ErrNoKeyConfig
	}
	inspect, err := k.Docker.ConfigInspect(ctx, name)
	if err != nil {
		return name, nil, fmt.Errorf("inspect key config %s: %w", name, err)
	}
	return name, inspect.Data, nil
}

// newest returns the highest-named config of the given kind (name suffixes are zero-padded
// unixnano, so lexicographic order is chronological). "" when none.
func (k *Kit) newest(ctx context.Context, kind string) (string, error) {
	names, err := k.Docker.ConfigList(ctx, LabelKind, kind)
	if err != nil {
		return "", fmt.Errorf("list %s configs: %w", kind, err)
	}
	if len(names) == 0 {
		return "", nil
	}
	sort.Strings(names)
	return names[len(names)-1], nil
}

// prune removes all but the KeepKeyConfigs newest key configs.
func (k *Kit) prune(ctx context.Context) error {
	names, err := k.Docker.ConfigList(ctx, LabelKind, KindKey)
	if err != nil {
		return fmt.Errorf("list %s configs for prune: %w", KindKey, err)
	}
	if len(names) <= KeepKeyConfigs {
		return nil
	}
	sort.Strings(names)
	for _, name := range names[:len(names)-KeepKeyConfigs] {
		if err := k.Docker.ConfigRemove(ctx, name); err != nil {
			return fmt.Errorf("prune %s: %w", name, err)
		}
	}
	return nil
}
