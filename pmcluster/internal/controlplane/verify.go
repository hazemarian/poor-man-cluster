package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// VerifyReport is what a control-plane restore drill measured.
type VerifyReport struct {
	SnapshotAt     time.Time `json:"snapshot_at"`
	SnapshotAge    string    `json:"snapshot_age"`
	StateConfig    string    `json:"state_config"`
	KeyConfig      string    `json:"key_config"`
	KeyFingerprint string    `json:"key_fingerprint"`
	Files          int       `json:"files"`
	Tables         int       `json:"tables"`
	SchemaVersion  string    `json:"schema_version"`
	Migrations     int       `json:"migrations"`
	Settings       int       `json:"settings"`
	Secrets        int       `json:"secrets"`
	SecretsOK      int       `json:"secrets_decrypted"`
	Configs        int       `json:"configs"`
	Warnings       []string  `json:"warnings,omitempty"`
}

// Verify restores the newest control-plane survivor kit into a throwaway
// directory and proves it works: the daemon's own store opens the DB, the
// schema is at the version this binary ships, and the key that came with the
// snapshot decrypts the secrets it came with.
//
// The backup path has had a weekly drill for a while; the control plane had
// none. Its kit — data.db and .encryption_key as two Raft-replicated Docker
// configs — was only ever exercised when a leader actually died, which is the
// worst possible moment to discover that the pair does not restore. This runs
// the restore without touching the live DataDir, so it is safe on a serving
// leader.
func (k *Kit) Verify(ctx context.Context) (*VerifyReport, error) {
	if k.Docker == nil {
		return nil, errors.New("verify needs swarm access (a manager): the kit lives in Raft-replicated configs")
	}
	stateName, err := k.newest(ctx, KindState)
	if err != nil {
		return nil, err
	}
	if stateName == "" {
		return nil, ErrNoSnapshots
	}
	inspect, err := k.Docker.ConfigInspect(ctx, stateName)
	if err != nil {
		return nil, fmt.Errorf("inspect state config %s: %w", stateName, err)
	}
	at, err := parseSnapshotAt(inspect.Labels)
	if err != nil {
		return nil, fmt.Errorf("state config %s: %w", stateName, err)
	}
	keyName := KeyNamePrefix + "_" + strings.TrimPrefix(stateName, StateNamePrefix+"_")
	keyInspect, err := k.Docker.ConfigInspect(ctx, keyName)
	if err != nil {
		return nil, fmt.Errorf("state config %s has no key config %s — this kit cannot be restored (a ciphertext without its key is not a backup); snapshot a node that holds the key", stateName, keyName)
	}

	rep := &VerifyReport{
		SnapshotAt:     at,
		SnapshotAge:    time.Since(at).Truncate(time.Second).String(),
		StateConfig:    stateName,
		KeyConfig:      keyName,
		KeyFingerprint: fingerprint(keyInspect.Data),
	}

	dir, err := os.MkdirTemp("", "pmcluster-kit-verify-")
	if err != nil {
		return nil, fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// Same extractor the real restore uses, pointed at the throwaway dir, so a
	// path-traversal entry or a truncated tar fails here rather than at 3am.
	tmpKit := &Kit{Docker: k.Docker, DataDir: dir, Log: k.Log}
	n, err := tmpKit.extractKit(inspect.Data, at)
	if err != nil {
		return nil, fmt.Errorf("extract kit: %w", err)
	}
	rep.Files = n
	if n == 0 {
		return nil, errors.New("kit extracted 0 files — the snapshot is not a data kit")
	}

	keyBytes := keyInspect.Data
	if len(keyBytes) != 32 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("key config holds %d bytes, want 32 — cannot decrypt with it", len(keyBytes)))
	} else if err := os.WriteFile(filepath.Join(dir, ".encryption_key"), keyBytes, 0o600); err != nil {
		return nil, fmt.Errorf("stage key: %w", err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, "data.db")); statErr != nil {
		return nil, fmt.Errorf("kit has no data.db: %w", statErr)
	}

	// Open through the daemon's own store: migrations, pragmas, schema check —
	// exactly what a promoted leader does with these bytes.
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		return nil, fmt.Errorf("open restored control-plane DB: %w", err)
	}
	defer func() { _ = st.Close() }()

	rep.Tables, rep.SchemaVersion, rep.Migrations = dbShape(ctx, st)
	if rep.Tables == 0 {
		return nil, errors.New("restored DB has no tables — the snapshot is empty or unreadable")
	}

	if n, err := countRows(ctx, st, "cluster_settings"); err == nil {
		rep.Settings = n
	} else {
		rep.Warnings = append(rep.Warnings, "count settings: "+err.Error())
	}
	if cfgs, err := st.ListConfigs(ctx, "", ""); err == nil {
		rep.Configs = len(cfgs)
	} else {
		rep.Warnings = append(rep.Warnings, "list configs: "+err.Error())
	}
	secs, err := st.ListSecrets(ctx, "", "")
	if err != nil {
		rep.Warnings = append(rep.Warnings, "list secrets: "+err.Error())
	}
	rep.Secrets = len(secs)

	// The point of the drill: the key in the kit opens the ciphertext in the
	// kit. A mismatch here means a promoted leader would come up unable to read
	// any credential it restored.
	if len(keyBytes) == 32 && len(secs) > 0 {
		cipher, cerr := credentials.Open(filepath.Join(dir, ".encryption_key"))
		if cerr != nil {
			rep.Warnings = append(rep.Warnings, "open restored key: "+cerr.Error())
		} else {
			for _, meta := range secs {
				row, rerr := st.Secret(ctx, meta.Name)
				if rerr != nil {
					rep.Warnings = append(rep.Warnings, fmt.Sprintf("read secret %s: %v", meta.Name, rerr))
					continue
				}
				if _, derr := cipher.Decrypt(row.Payload); derr != nil {
					rep.Warnings = append(rep.Warnings, fmt.Sprintf("secret %s does not decrypt with the key stored beside it", meta.Name))
					continue
				}
				rep.SecretsOK++
			}
		}
	}

	return rep, nil
}

// dbShape reports the table count, the newest applied migration and how many
// migrations the restored DB carries. Versions are zero-padded file names
// (0001_init …), so ordering by version is ordering by application — ordering
// by applied_at is not: every migration of a fresh DB is stamped in the same
// second and the "newest" row comes back arbitrary.
func dbShape(ctx context.Context, st *store.Store) (tables int, version string, migrations int) {
	rows, err := st.DB().QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name string
			if rows.Scan(&name) == nil {
				tables++
			}
		}
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT version FROM schema_version ORDER BY version DESC LIMIT 1`).Scan(&version); err != nil {
		version = ""
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_version`).Scan(&migrations); err != nil {
		migrations = 0
	}
	return tables, version, migrations
}

// fingerprint is the first 8 hex characters of the key's sha256 — enough to
// compare two keys, useless for recovering one.
func fingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])[:8]
}

// countRows counts a table without going through a typed accessor — verify
// reports shape, not content.
func countRows(ctx context.Context, st *store.Store, table string) (int, error) {
	var n int
	if err := st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
