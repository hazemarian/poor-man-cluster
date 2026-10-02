// Package controlplane implements L2 of the failover story: the swarm leader
// snapshots the control-plane survivor kit into Raft-replicated Docker
// configs, so a standby manager promoted to leader can restore the control
// plane without depending on the per-node tarball archive being shipped to it
// (the old rsync/tarball dependency).
//
// The survivor kit is everything a promoted node needs to serve the same
// control plane: data.db (users, API keys / token hashes, credential
// ciphertexts, settings, webhook secrets, stack revisions), config.yaml, and
// the rendered config/ tree (TLS certs, stack pointers).
//
// Security split: the AES-GCM encryption key (.encryption_key) NEVER shares a
// config blob with the ciphertext it unlocks. Two config families are kept:
//
//	pmcluster_state_<unixnano>     — the data kit (tar.gz), all of the above
//	pmcluster_state_key_<unixnano> — the .encryption_key bytes, nothing else
//
// Swarm replicates both to every manager automatically; a manager's Raft store
// is the transport, so no host-to-host file shipping is involved.
package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
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

// Docker config names. Configs are immutable, so every snapshot creates a new
// name with a zero-padded unixnano suffix; lexicographic ordering of the name
// IS chronological ordering, which prune and restore rely on.
const (
	// StateNamePrefix names the config carrying the data kit.
	StateNamePrefix = "pmcluster_state"
	// KeyNamePrefix names the config carrying only the encryption key.
	KeyNamePrefix = "pmcluster_state_key"
)

// Label keys and values carried on both config families.
const (
	// ManagedLabel marks pmcluster-managed configs (same value as
	// cluster.PmclusterLabel; kept local to avoid an import cycle).
	ManagedLabel = "io.pmcluster.managed"
	// LabelKind distinguishes the state config from the key config.
	LabelKind = "pmcluster.kind"
	// KindState marks the data-kit config.
	KindState = "state"
	// KindKey marks the key-only config.
	KindKey = "state-key"
	// LabelSnapshotAt carries the RFC3339Nano snapshot time.
	LabelSnapshotAt = "pmcluster.snapshot-at"
	// LabelVersion mirrors the pmcluster.version label on the other managed
	// configs so the version-drift check can identify them.
	LabelVersion = "pmcluster.version"
)

const (
	// MaxConfigBytes is the ceiling for a snapshot payload. Docker rejects
	// configs above 500 KiB; the control plane is small, so the guard exists
	// to fail loudly rather than halfway.
	MaxConfigBytes = 500 << 10
	// KeepSnapshots is how many snapshots per family are retained. History of
	// two keeps the newest available to a standby that read an older name
	// while a prune raced a create.
	KeepSnapshots = 2
	// DefaultSnapshotInterval is how often the leader re-checks for changes.
	DefaultSnapshotInterval = 5 * time.Minute
)

// ErrNoSnapshots is returned by Restore when no pmcluster_state_* config
// exists (or Docker is unavailable) — the caller falls back to the tarball
// archive path for clusters that predate Raft config snapshots.
var ErrNoSnapshots = errors.New("no control-plane state config exists")

// Kit snapshots and restores the control-plane survivor kit via Docker
// configs. Create one per daemon; Snapshot must only be called on the swarm
// leader (Raft replication is the transport), while Restore is safe on any
// manager.
type Kit struct {
	Docker  runtime.Client
	DataDir string
	Log     zerolog.Logger
}

// DBPath returns the SQLite database path inside DataDir.
func (k *Kit) DBPath() string { return filepath.Join(k.DataDir, "data.db") }

// KeyPath returns the encryption key path inside DataDir.
func (k *Kit) KeyPath() string { return filepath.Join(k.DataDir, ".encryption_key") }

// Snapshot writes the current survivor kit into fresh state + key configs
// (Raft-replicated to every manager) when the kit changed since the last
// snapshot, then prunes old snapshots. It is a no-op when Docker is
// unavailable, the kit is uninitialised (no data.db), or nothing changed —
// the leader loop calls it on every tick and every promotion.
func (k *Kit) Snapshot(ctx context.Context) error {
	if k.Docker == nil {
		return nil // standalone/local mode: nothing to replicate to
	}
	dbPath := k.DBPath()
	if _, err := os.Stat(dbPath); err != nil {
		return nil // control plane not initialised; nothing to snapshot
	}
	log := k.Log.With().Str("component", "controlplane").Logger()

	changed, err := k.needsSnapshot(ctx)
	if err != nil {
		return err
	}
	if !changed {
		log.Debug().Msg("control-plane state unchanged; skipping snapshot")
		return nil
	}

	kit, err := k.readKit()
	if err != nil {
		return fmt.Errorf("read control-plane kit: %w", err)
	}
	if len(kit) > MaxConfigBytes {
		return fmt.Errorf("control-plane state payload %d bytes exceeds the %d-byte Docker config limit — the data dir has grown too large for Raft config snapshots; reduce data.db growth or rely on the tarball/offsite backup path", len(kit), MaxConfigBytes)
	}

	key, err := os.ReadFile(k.KeyPath())
	if err != nil {
		return fmt.Errorf("read control-plane encryption key: %w", err)
	}

	now := time.Now()
	ts := strconv.FormatInt(now.UnixNano(), 10)

	stateLabels := map[string]string{
		ManagedLabel:    "true",
		LabelKind:       KindState,
		LabelSnapshotAt: now.Format(time.RFC3339Nano),
		LabelVersion:    buildinfo.Version,
	}
	stateName := StateNamePrefix + "_" + ts
	if err := k.Docker.ConfigCreate(ctx, runtime.ConfigSpec{Name: stateName, Data: kit, Labels: stateLabels}); err != nil {
		return fmt.Errorf("create control-plane state config: %w", err)
	}
	log.Info().Str("config", stateName).Int("bytes", len(kit)).Msg("control-plane state snapshotted to Raft-replicated config")

	keyLabels := map[string]string{
		ManagedLabel:    "true",
		LabelKind:       KindKey,
		LabelSnapshotAt: now.Format(time.RFC3339Nano),
		LabelVersion:    buildinfo.Version,
	}
	keyName := KeyNamePrefix + "_" + ts
	if err := k.Docker.ConfigCreate(ctx, runtime.ConfigSpec{Name: keyName, Data: key, Labels: keyLabels}); err != nil {
		return fmt.Errorf("create control-plane key config: %w", err)
	}
	log.Info().Str("config", keyName).Int("bytes", len(key)).Msg("control-plane encryption key snapshotted to Raft-replicated config")

	if err := k.prune(ctx, KindState); err != nil {
		log.Warn().Err(err).Msg("prune control-plane state configs had issues")
	}
	if err := k.prune(ctx, KindKey); err != nil {
		log.Warn().Err(err).Msg("prune control-plane key configs had issues")
	}
	return nil
}

// needsSnapshot reports whether the kit changed since the newest state
// snapshot: any kit file (data.db, config.yaml, config/, .encryption_key) with
// an mtime newer than the newest snapshot's timestamp. True when no state
// config exists yet.
func (k *Kit) needsSnapshot(ctx context.Context) (bool, error) {
	newest, err := k.newest(ctx, KindState)
	if err != nil {
		return false, err
	}
	if newest == "" {
		return true, nil
	}
	inspect, err := k.Docker.ConfigInspect(ctx, newest)
	if err != nil {
		return false, fmt.Errorf("inspect newest state config %s: %w", newest, err)
	}
	at, err := parseSnapshotAt(inspect.Labels)
	if err != nil {
		return false, fmt.Errorf("newest state config %s: %w", newest, err)
	}
	mtime, err := k.maxKitMtime()
	if err != nil {
		return false, err
	}
	return mtime.After(at), nil
}

// maxKitMtime returns the newest mtime among the kit files (data.db,
// config.yaml, config/ tree, and the encryption key). The zero time is
// returned when none exist.
func (k *Kit) maxKitMtime() (time.Time, error) {
	var max time.Time
	err := filepath.Walk(k.DataDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			rel, _ := filepath.Rel(k.DataDir, path)
			if rel == "logs" {
				return filepath.SkipDir
			}
			return nil
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		if fi.ModTime().After(max) {
			max = fi.ModTime()
		}
		return nil
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("walk data dir for kit mtimes: %w", err)
	}
	return max, nil
}

// readKit tars + gzips the data kit: data.db, config.yaml, and config/ (the
// rendered configs + TLS certs). logs/ and .encryption_key are excluded — the
// key lives in its own config per the security split, and logs are local
// noise, not survivor state. Entry paths are relative to DataDir.
func (k *Kit) readKit() ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	base, err := filepath.Abs(k.DataDir)
	if err != nil {
		return nil, err
	}
	err = filepath.Walk(base, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if rel == "logs" || strings.HasPrefix(rel, "logs"+string(filepath.Separator)) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == ".encryption_key" {
			return nil // security split: key goes to its own config
		}
		name := filepath.ToSlash(rel)
		switch {
		case fi.IsDir():
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: int64(fi.Mode().Perm()), Typeflag: tar.TypeDir, ModTime: fi.ModTime()}); err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			hdr := &tar.Header{Name: name, Mode: int64(fi.Mode().Perm()), Size: fi.Size(), Typeflag: tar.TypeReg, ModTime: fi.ModTime()}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			if _, err := io.Copy(tw, f); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("tar data dir: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Restore brings the control plane up to the newest snapshot when the local
// data.db is missing or older than it, and repairs a missing encryption key
// even when the DB is current. Returns (restored, err): restored reports
// whether anything was written. ErrNoSnapshots is returned when no state
// config exists — the caller falls back to the tarball archive path.
func (k *Kit) Restore(ctx context.Context) (bool, error) {
	if k.Docker == nil {
		return false, ErrNoSnapshots // standalone/local mode: no Raft configs to read
	}
	log := k.Log.With().Str("component", "controlplane").Logger()

	newest, err := k.newest(ctx, KindState)
	if err != nil {
		return false, err
	}
	if newest == "" {
		log.Debug().Msg("no control-plane state config; nothing to restore from Raft")
		return false, ErrNoSnapshots
	}
	inspect, err := k.Docker.ConfigInspect(ctx, newest)
	if err != nil {
		return false, fmt.Errorf("inspect newest state config %s: %w", newest, err)
	}
	at, err := parseSnapshotAt(inspect.Labels)
	if err != nil {
		return false, fmt.Errorf("newest state config %s: %w", newest, err)
	}

	dbPath := k.DBPath()
	dbInfo, dbErr := os.Stat(dbPath)
	needDB := os.IsNotExist(dbErr) || (dbErr == nil && dbInfo.ModTime().Before(at))
	if dbErr != nil && !os.IsNotExist(dbErr) {
		return false, fmt.Errorf("stat local data.db: %w", dbErr)
	}

	keyName := KeyNamePrefix + "_" + strings.TrimPrefix(newest, StateNamePrefix+"_")
	keyInspect, keyErr := k.Docker.ConfigInspect(ctx, keyName)

	_, keyMissing := os.Stat(k.KeyPath())
	needKey := os.IsNotExist(keyMissing)

	switch {
	case needDB:
		// Full restore. The key is mandatory: a data kit without its key is a
		// control plane that cannot decrypt its own credentials.
		if keyErr != nil {
			return false, fmt.Errorf("state config %s exists but key config %s is missing — refusing to restore a ciphertext without its key (security split); investigate before promoting this node", newest, keyName)
		}
		n, err := k.extractKit(inspect.Data, at)
		if err != nil {
			return false, fmt.Errorf("restore control plane from %s: %w", newest, err)
		}
		if err := os.WriteFile(k.KeyPath(), keyInspect.Data, 0o600); err != nil {
			return false, fmt.Errorf("restore control-plane encryption key: %w", err)
		}
		if err := os.Chtimes(k.KeyPath(), at, at); err != nil {
			return false, fmt.Errorf("timestamp restored encryption key: %w", err)
		}
		if err := os.Chtimes(dbPath, at, at); err != nil {
			return false, fmt.Errorf("timestamp restored data.db: %w", err)
		}
		log.Info().Str("config", newest).Str("snapshot-at", at.Format(time.RFC3339Nano)).Int("files", n).Msg("control-plane data restored from Raft config")
		return true, nil
	case needKey:
		// DB is current but this node never got the encryption key (fresh
		// join, or the key file was lost). Repair just the key.
		if keyErr != nil {
			log.Warn().Str("config", newest).Msg("local data.db is current but no key config found; leaving encryption key missing (webhook/credential features will stay disabled)")
			return false, nil
		}
		if err := os.WriteFile(k.KeyPath(), keyInspect.Data, 0o600); err != nil {
			return false, fmt.Errorf("restore missing control-plane encryption key: %w", err)
		}
		if err := os.Chtimes(k.KeyPath(), at, at); err != nil {
			return false, fmt.Errorf("timestamp restored encryption key: %w", err)
		}
		log.Info().Str("config", keyName).Msg("restored missing control-plane encryption key from Raft config")
		return true, nil
	default:
		log.Info().Str("config", newest).Msg("local control-plane DB is current (shared storage?); no restore from Raft config")
		return false, nil
	}
}

// extractKit gunzips + untars the state config payload into DataDir. data.db
// is timestamped at the snapshot time so the next freshness check sees an
// up-to-date DB (an older mtime would re-restore on every promotion); the
// other kit files keep their snapshot mtimes. Entry paths are validated
// against escaping DataDir.
func (k *Kit) extractKit(payload []byte, at time.Time) (int, error) {
	gz, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return 0, fmt.Errorf("gzip open: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	base, err := filepath.Abs(k.DataDir)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return 0, err
	}
	var count int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, err
		}
		name := filepath.Join(base, filepath.FromSlash(hdr.Name))
		if name != base && !strings.HasPrefix(name, base+string(filepath.Separator)) {
			return count, fmt.Errorf("config entry escapes data dir: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(name, 0o755); err != nil {
				return count, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return count, err
			}
			out, err := os.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return count, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return count, err
			}
			out.Close()
			mt := hdr.ModTime
			if filepath.Base(name) == "data.db" {
				mt = at
			}
			if err := os.Chtimes(name, mt, mt); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, nil
}

// Loop re-checks the kit for changes on every tick and snapshots when needed,
// until ctx is cancelled. The swarm leader runs this alongside the reconcile
// loop so a mutation is captured within one interval even when no reconcile
// event fires. A no-op when Docker is unavailable.
func (k *Kit) Loop(ctx context.Context, interval time.Duration) {
	if k.Docker == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultSnapshotInterval
	}
	log := k.Log.With().Str("component", "controlplane").Logger()
	t := time.NewTicker(interval)
	defer t.Stop()
	log.Debug().Dur("interval", interval).Msg("control-plane snapshot loop armed (leader)")
	for {
		select {
		case <-ctx.Done():
			log.Debug().Msg("control-plane snapshot loop stopped")
			return
		case <-t.C:
			if err := k.Snapshot(ctx); err != nil {
				log.Warn().Err(err).Msg("control-plane snapshot had issues")
			}
		}
	}
}

// newest returns the highest-named config of the given kind (name suffixes are
// zero-padded unixnano, so lexicographic order is chronological). "" when none.
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

// prune removes all but the KeepSnapshots newest configs of a kind.
func (k *Kit) prune(ctx context.Context, kind string) error {
	names, err := k.Docker.ConfigList(ctx, LabelKind, kind)
	if err != nil {
		return fmt.Errorf("list %s configs for prune: %w", kind, err)
	}
	if len(names) <= KeepSnapshots {
		return nil
	}
	sort.Strings(names)
	for _, name := range names[:len(names)-KeepSnapshots] {
		if err := k.Docker.ConfigRemove(ctx, name); err != nil {
			return fmt.Errorf("prune %s: %w", name, err)
		}
	}
	return nil
}

// parseSnapshotAt reads the RFC3339Nano snapshot timestamp off a config's
// labels.
func parseSnapshotAt(labels map[string]string) (time.Time, error) {
	if labels == nil {
		return time.Time{}, fmt.Errorf("missing %s label", LabelSnapshotAt)
	}
	raw := labels[LabelSnapshotAt]
	if raw == "" {
		return time.Time{}, fmt.Errorf("missing %s label", LabelSnapshotAt)
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse %s label %q: %w", LabelSnapshotAt, raw, err)
	}
	return t, nil
}
