package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Snapshot is the mirrorable projection of this store: the cluster state a
// follower manager needs in order to answer reads on its own, and nothing else.
//
// # What is in it, and what is deliberately not
//
// The set is closed and small — settings, secrets, users — because these tables
// are tens of rows and a follower needs all of them. Deletes come free: a row
// absent from a FULL snapshot is deleted locally, which a delta cannot express
// without tombstones.
//
// Deliberately absent, and each for a reason of its own:
//
//   - backups, webhook_deliveries, stack_status, node_stats, github_runners —
//     logs, audit history, reconcile bookkeeping, per-node samples and per-node
//     observations. A follower's copy would describe another machine's containers
//     or an archive it cannot see.
//   - stack_revisions / config_versions: history, and the one table that grows
//     without bound. The control-plane kit already carries a windowed copy for a
//     promoted leader; a mirror does not need a rollback archive.
//   - users.last_used_at: a mirrored timestamp would be rewritten by the leader on
//     every authenticated request, so every poll would be a 200 that changes a row
//     for no state change. It is therefore not part of the snapshot, it does not
//     advance the version, and `TouchUser` is the one users write that does not
//     bump. Mirroring it would also make a follower's "last used" describe
//     traffic it never served.
//
// # Names
//
// The brief and the ADR talk in domain terms; the store's tables are the real
// names, and the mapping is worth stating once, here, because a reader looking
// for a `storage_nodes` table will not find one:
//
//	settings        → cluster_settings (key/value; `storage_nodes` is a KEY in it,
//	                  not a table of its own)
//	secrets         → secrets (AES-GCM ciphertext payload + sha256 hash, never
//	                  plaintext — the key that decrypts it is provisioned at join,
//	                  never carried by this snapshot)
//	users           → users (which IS the API-key table: `apikeys` mints a row here
//	                  per token, so the brief's "users" and "api_keys" are one table)
type Snapshot struct {
	// Version is the cluster_version counter this snapshot was read at.
	Version int64
	// GeneratedAt is the unix second stamped by the write that produced Version.
	GeneratedAt int64
	// Settings, Secrets and Users are ordered deterministically and are never nil,
	// so an empty table marshals to [] — a follower must be able to read "this table
	// is empty" and delete every row locally. A nil (null) table would be
	// indistinguishable from "the leader did not send it".
	Settings []SnapshotSetting
	Secrets  []SnapshotSecret
	Users    []SnapshotUser
}

// SnapshotSetting is one cluster_settings row: the platform's knobs, among which
// a few hold secret VALUES (the offsite S3 keys, the SSO client secret) that the
// console masks. They travel only over the node-certificate channel and only to
// managers — the same machines that already hold them in the Raft control-plane
// kit.
type SnapshotSetting struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// SnapshotSecret is one secrets row, ciphertext included: a follower stores the
// payload as it is and decrypts with the key it holds locally. Sending plaintext
// would undo the point of encrypting at rest; sending only the hash would leave a
// mirror that cannot render a stack.
type SnapshotSecret struct {
	ID    int64  `json:"id"`
	Scope string `json:"scope"`
	Stack string `json:"stack"`
	Name  string `json:"name"`
	// Payload is AES-GCM ciphertext (nonce || seal), base64 in JSON.
	Payload   []byte `json:"payload"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"created_at"`
	SwarmRev  int64  `json:"swarm_rev"`
}

// SnapshotUser is one users row — a daemon API token. The token itself is not
// recoverable from it: TokenHash is an argon2id hash and TokenID the public index
// prefix. A follower needs both to authenticate the same bearer tokens locally.
type SnapshotUser struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	TokenID   string `json:"token_id"`
	TokenHash string `json:"token_hash"`
	Role      string `json:"role"`
	Stack     string `json:"stack"`
	CreatedAt int64  `json:"created_at"`
}

// Snapshot reads the whole projection at ONE version.
//
// The read runs in a transaction so the version and the rows come from the same
// state: bumping the counter is part of the writer's transaction, so a snapshot
// can never pair a new version with a half-applied write (or an old version with a
// row the counter does not describe). Under SQLite's one-writer rule this is a
// consistent read of a point in time.
func (s *Store) Snapshot(ctx context.Context) (*Snapshot, error) {
	tx, err := s.conn().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("snapshot: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	snap := &Snapshot{
		Settings: []SnapshotSetting{},
		Secrets:  []SnapshotSecret{},
		Users:    []SnapshotUser{},
	}
	if err := tx.QueryRowContext(ctx, clusterVersionSQL).Scan(&snap.Version, &snap.GeneratedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("snapshot: cluster version row missing (migration 0030 did not seed it)")
		}
		return nil, fmt.Errorf("snapshot: read cluster version: %w", err)
	}

	if snap.Settings, err = snapshotSettings(ctx, tx); err != nil {
		return nil, err
	}
	if snap.Secrets, err = snapshotSecrets(ctx, tx); err != nil {
		return nil, err
	}
	if snap.Users, err = snapshotUsers(ctx, tx); err != nil {
		return nil, err
	}
	return snap, nil
}

// snapshotSettings reads cluster_settings ordered by key, so the same version
// always produces the same bytes (diffable, and a future delta has a stable order
// to compare against).
func snapshotSettings(ctx context.Context, tx *sql.Tx) ([]SnapshotSetting, error) {
	rows, err := tx.QueryContext(ctx, `SELECT key, value FROM cluster_settings ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("snapshot: query settings: %w", err)
	}
	defer rows.Close()
	out := []SnapshotSetting{}
	for rows.Next() {
		var r SnapshotSetting
		if err := rows.Scan(&r.Key, &r.Value); err != nil {
			return nil, fmt.Errorf("snapshot: scan setting: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snapshot: settings: %w", err)
	}
	return out, nil
}

func snapshotSecrets(ctx context.Context, tx *sql.Tx) ([]SnapshotSecret, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, scope, stack, name, payload, hash, created_at, swarm_rev FROM secrets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("snapshot: query secrets: %w", err)
	}
	defer rows.Close()
	out := []SnapshotSecret{}
	for rows.Next() {
		var r SnapshotSecret
		if err := rows.Scan(&r.ID, &r.Scope, &r.Stack, &r.Name, &r.Payload, &r.Hash, &r.CreatedAt, &r.SwarmRev); err != nil {
			return nil, fmt.Errorf("snapshot: scan secret: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snapshot: secrets: %w", err)
	}
	return out, nil
}

// snapshotUsers reads users ordered by id — not by name, because id is what the
// follower upserts on and what a token's TokenID maps to.
//
// token_id is nullable (a legacy token has none) and is flattened to "" here: the
// domain models in this repo carry no sql.Null*.
func snapshotUsers(ctx context.Context, tx *sql.Tx) ([]SnapshotUser, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, name, token_id, token_hash, role, stack, created_at FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("snapshot: query users: %w", err)
	}
	defer rows.Close()
	out := []SnapshotUser{}
	for rows.Next() {
		var r SnapshotUser
		var tokenID sql.NullString
		if err := rows.Scan(&r.ID, &r.Name, &tokenID, &r.TokenHash, &r.Role, &r.Stack, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("snapshot: scan user: %w", err)
		}
		r.TokenID = tokenID.String
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snapshot: users: %w", err)
	}
	return out, nil
}
