package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Snapshot is the mirrorable state of this store: the cluster's whole control-plane
// document, read at one version.
//
// # What is in it, and what is deliberately not
//
// Every table a follower needs, minus an explicit exclusion set in mirror_tables.go
// whose reasons are all the same: a row that describes the NODE it lives on (an
// archive index, a resource sample, a runner observation, the mirror's own
// bookkeeping) never travels, because carrying it would make the receiving machine
// claim something it never observed.
//
// It is the WHOLE document and not a projection because adoption no longer copies a
// file (ADR 0005): a manager promoted to leader has nothing but the store it has been
// mirroring, so a mirror carrying settings, secrets and users would promote a leader
// that cannot read its own stacks. Deletes come free the same way they always did: a
// row absent from a FULL snapshot is deleted locally, which a delta cannot express
// without tombstones.
//
// `users.last_used_at` travels with the users row. It changes without bumping the
// version (TouchUser is the one users write that does not), so a follower's copy is
// only as new as the last write that did bump — provenance, not a claim that the
// follower served that traffic.
//
// # Names
//
// The tables are the store's real names. A reader looking for `storage_nodes` will not
// find one: it is a KEY in cluster_settings. The `users` table IS the API-key table —
// `apikeys` mints a row here per token.
type Snapshot struct {
	// Version is the cluster_version counter this snapshot was read at.
	Version int64
	// GeneratedAt is the unix second stamped by the write that produced Version.
	GeneratedAt int64
	// Tables is the whole mirrored document, keyed by the store's table name. Every
	// mirrored table is present, empty ones included: a follower must be able to read
	// "this table is empty" and delete every row locally, and an absent key would be
	// indistinguishable from "the leader did not send it".
	Tables map[string]TableData
	// Settings, Secrets and Users are the pre-document projection, derived from Tables.
	// They are still populated (and still on the wire) for a follower running a binary
	// that predates Tables: without them such a node would decode its three arrays as
	// nil and delete every row it mirrors. Delete them once the fleet is upgraded.
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

	snap := &Snapshot{}
	if err := tx.QueryRowContext(ctx, clusterVersionSQL).Scan(&snap.Version, &snap.GeneratedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("snapshot: cluster version row missing (migration 0030 did not seed it)")
		}
		return nil, fmt.Errorf("snapshot: read cluster version: %w", err)
	}

	tables, err := readMirrorTables(ctx, tx)
	if err != nil {
		return nil, err
	}
	snap.Tables = tables
	// One read, two shapes: the document is the source, and the pre-document projection
	// is derived from it so the two can never describe different rows.
	snap.Settings = legacySettings(tables)
	snap.Secrets = legacySecrets(tables)
	snap.Users = legacyUsers(tables)
	return snap, nil
}
