package store

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// TestMirrorTables_EveryTableIsClassified is the guard that makes the exclusion set
// real rather than prose: every table the migrations create must appear in exactly ONE
// of mirroredTables and excludedTables, and every exclusion must say why.
//
// It is the same shape as the kit's classification test (which this mirror replaces),
// and it exists because the failure it prevents is silent: a migration adds a table, the
// mirror does not carry it, and every follower is quietly missing cluster state until
// the day it is promoted.
func TestMirrorTables_EveryTableIsClassified(t *testing.T) {
	st := openTestStore(t)
	tables := schemaTables(t, st)
	if len(tables) < 10 {
		t.Fatalf("found %d tables; the schema should have a dozen or more — the query is wrong", len(tables))
	}

	mirrored := map[string]bool{}
	for _, name := range mirroredTables {
		if mirrored[name] {
			t.Errorf("table %q is listed twice in mirroredTables", name)
		}
		mirrored[name] = true
	}

	for _, name := range tables {
		_, excluded := excludedTables[name]
		switch {
		case mirrored[name] && excluded:
			t.Errorf("table %q is both mirrored and excluded — pick one; a table in both lists means the answer depends on which list a reader consults", name)
		case !mirrored[name] && !excluded:
			t.Errorf("table %q is neither mirrored nor excluded: add it to mirroredTables (a follower needs it) or to excludedTables with the reason it describes only the node it lives on", name)
		}
	}

	// A listed table that does not exist is a typo that would fail at runtime on the
	// first poll, where nobody reads the error.
	for _, name := range mirroredTables {
		if !containsString(tables, name) {
			t.Errorf("mirroredTables names %q, which no migration creates", name)
		}
	}

	// An exclusion without a reason is how a table gets dropped from a mirror by
	// accident: the next reader has nothing to disagree with.
	for name, reason := range excludedTables {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("excludedTables[%q] has no reason; every exclusion states why its rows describe only this node", name)
		}
		if !containsString(tables, name) {
			t.Errorf("excludedTables names %q, which no migration creates", name)
		}
	}

	// The mirror's own bookkeeping must never be a candidate: a promoted leader that
	// inherited a mirror's "we are current" claim would lie about its own freshness.
	if mirrored["sync_state"] {
		t.Error("sync_state is mirrored; it is this node's own bookkeeping and must stay local")
	}
}

// TestApplySnapshot_CarriesATableTheProjectionDidNot is the whole point of the widened
// mirror, measured end to end: a stack and its revision — the table the old projection
// never carried and the one that grows without bound — must arrive on the follower, with
// the pointer the promoted leader needs.
func TestApplySnapshot_CarriesATableTheProjectionDidNot(t *testing.T) {
	leader := openTestStore(t)
	follower := openTestStore(t)
	ctx := context.Background()

	rev := &StackRevision{
		StackName:    "web",
		Revision:     7,
		SourceYAML:   "source-yaml",
		RenderedYAML: "rendered-yaml",
		RenderedHash: "abc123",
		SourceFile:   "deploy/web.yaml",
	}
	if err := leader.RecordDeploy(ctx, rev, "https://example.test/repo"); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}
	if err := leader.CreateWebhookSource(ctx, "ci", "ci hook", []byte{0x00, 0xff, 0x10}); err != nil {
		t.Fatalf("CreateWebhookSource: %v", err)
	}
	if err := leader.SetSetting(ctx, "domain", "example.test"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	snap, err := leader.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := follower.ApplySnapshot(ctx, snap, "leader-1", 1_700_000_000); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}

	stack, err := follower.Stack(ctx, "web")
	if err != nil {
		t.Fatalf("Stack on the follower: %v", err)
	}
	if stack.CurrentRevision != 7 {
		t.Errorf("current_revision = %d, want 7 — a promoted leader reads its live manifest through this pointer", stack.CurrentRevision)
	}
	got, err := follower.Revision(ctx, "web", 7)
	if err != nil {
		t.Fatalf("Revision on the follower: %v", err)
	}
	if got.RenderedYAML != "rendered-yaml" || got.RenderedHash != "abc123" || got.SourceFile != "deploy/web.yaml" {
		t.Errorf("revision = %+v, want the leader's row verbatim", got)
	}

	// A BLOB must survive the JSON round trip as bytes: base64 in the wire, the same
	// bytes on the other side. A mirror that turned ciphertext into a base64 STRING
	// would still "work" for hashing and fail at decryption.
	src, err := follower.WebhookSource(ctx, "ci")
	if err != nil {
		t.Fatalf("WebhookSource on the follower: %v", err)
	}
	if len(src.SecretCiphertext) != 3 || src.SecretCiphertext[0] != 0x00 || src.SecretCiphertext[2] != 0x10 {
		t.Errorf("secret ciphertext = %v, want the leader's bytes", src.SecretCiphertext)
	}
	// mirrored_at is this node's ARRIVAL time, not the leader's: the leader's copy says 0
	// (nothing mirrored it there), and a follower that copied that number would claim its
	// rows arrived before they did.
	var mirroredAt int64
	if err := follower.DB().QueryRowContext(ctx,
		`SELECT mirrored_at FROM cluster_settings WHERE key = 'domain'`).Scan(&mirroredAt); err != nil {
		t.Fatalf("read mirrored_at: %v", err)
	}
	if mirroredAt != 1_700_000_000 {
		t.Errorf("setting mirrored_at = %d, want the apply's own clock", mirroredAt)
	}
	state, err := follower.SyncState(ctx)
	if err != nil {
		t.Fatalf("SyncState: %v", err)
	}
	if state.Leader != "leader-1" || state.Version != snap.Version || state.AppliedAt != 1_700_000_000 {
		t.Errorf("sync state = %+v, want leader-1 at the applied version and time", state)
	}
}

// TestApplySnapshot_DeleteIsVisibleAsAbsence: a full snapshot expresses a delete by
// omission. The row exists on the follower, the leader stops sending it, and the
// follower must not keep it — the half of the apply that an upsert-only mirror gets
// wrong, and the reason these are full snapshots rather than deltas.
func TestApplySnapshot_DeleteIsVisibleAsAbsence(t *testing.T) {
	leader := openTestStore(t)
	follower := openTestStore(t)
	ctx := context.Background()

	rev := &StackRevision{StackName: "web", Revision: 1, RenderedYAML: "one", RenderedHash: "h1"}
	if err := leader.RecordDeploy(ctx, rev, ""); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}
	first, err := leader.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := follower.ApplySnapshot(ctx, first, "leader-1", 1_700_000_000); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}
	if _, err := follower.Revision(ctx, "web", 1); err != nil {
		t.Fatalf("the stack revision did not arrive: %v", err)
	}

	// The operator deletes the stack on the leader. The next full snapshot omits it.
	if err := leader.DeleteStack(ctx, "web"); err != nil {
		t.Fatalf("DeleteStack: %v", err)
	}
	second, err := leader.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := follower.ApplySnapshot(ctx, second, "leader-1", 1_700_000_100); err != nil {
		t.Fatalf("ApplySnapshot (after the delete): %v", err)
	}
	if _, err := follower.Stack(ctx, "web"); err == nil {
		t.Error("the follower still answers a stack the leader deleted — a promoted leader would serve a stack nobody owns")
	}
	if _, err := follower.Revision(ctx, "web", 1); err == nil {
		t.Error("the follower still carries a stack revision the leader deleted")
	}
}

// TestApplySnapshot_LeavesTablesItWasNotSentAlone is the rolling-upgrade rule: a leader on
// an older binary sends only the pre-document projection, and a follower must keep every
// other table rather than clearing what nobody sent.
func TestApplySnapshot_LeavesTablesItWasNotSentAlone(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.RecordDeploy(ctx, &StackRevision{StackName: "kept", Revision: 1, RenderedHash: "h"}, ""); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}

	// Exactly what an old leader sends: three tables, no document.
	old := &Snapshot{
		Version:     2,
		GeneratedAt: 1_700_000_000,
		Settings:    []SnapshotSetting{{Key: "domain", Value: "example.test"}},
		Secrets:     []SnapshotSecret{},
		Users:       []SnapshotUser{},
	}
	if err := st.ApplySnapshot(ctx, old, "old-leader", 1_700_000_000); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}
	if _, err := st.Stack(ctx, "kept"); err != nil {
		t.Errorf("a table the document did not name was cleared: %v", err)
	}
}

// TestApplySnapshot_RefusesATableThisBinaryDoesNotMirror: a leader ahead of this follower
// sends a table this binary has never heard of. Applying what it can and ignoring the rest
// would leave the follower silently incomplete, so the poll fails and the last applied
// snapshot stays.
func TestApplySnapshot_RefusesATableThisBinaryDoesNotMirror(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	snap := &Snapshot{
		Version:     1,
		GeneratedAt: 1_700_000_000,
		Tables: map[string]TableData{
			"not_a_table": {Columns: []string{"id"}, Types: []string{"INTEGER"}, Rows: [][]any{}},
		},
	}
	err := st.ApplySnapshot(ctx, snap, "leader-1", 1_700_000_000)
	if err == nil {
		t.Fatal("ApplySnapshot accepted a table this binary does not mirror")
	}
	if !strings.Contains(err.Error(), "not_a_table") {
		t.Errorf("the refusal does not name the table: %v", err)
	}
	if _, err := st.SyncState(ctx); err == nil {
		t.Error("a refused apply recorded a sync state — this node would now claim to be current")
	}
}

func schemaTables(t *testing.T, st *Store) []string {
	t.Helper()
	rows, err := st.DB().Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	sort.Strings(out)
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
