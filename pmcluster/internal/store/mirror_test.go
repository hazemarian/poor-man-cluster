package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestMirrorReadingNeverSynced: a node that has never applied a snapshot is not an error
// condition, it is a fact each surface reports differently (a leader is the source, a
// worker has no store, a fresh follower has not polled yet).
func TestMirrorReadingNeverSynced(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.MirrorReading(context.Background(), time.Now()); !errors.Is(err, ErrNeverSynced) {
		t.Fatalf("MirrorReading on a fresh store = %v, want ErrNeverSynced", err)
	}
}

// TestMirrorReadingReportsWhoseStateHowOldAndHowMuch is the label every read surface
// prints. The age is measured from applied_at on THIS node's clock, and the counts are
// what make it an answer rather than a version number.
func TestMirrorReadingReportsWhoseStateHowOldAndHowMuch(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	applied := int64(1_700_000_000)
	if err := st.ApplySnapshot(ctx, &Snapshot{
		Version:     9,
		GeneratedAt: applied - 2,
		Settings:    []SnapshotSetting{set("domain", "example.test"), set("volume_root", "/var/stack/data")},
		Secrets:     []SnapshotSecret{sec(1, "sso_client_secret")},
		Users:       []SnapshotUser{usr(1, "edge", "ab12")},
	}, "nxt-sw-5-m", applied); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}

	m, err := st.MirrorReading(ctx, time.Unix(applied+42, 0))
	if err != nil {
		t.Fatalf("MirrorReading: %v", err)
	}
	if m.Leader != "nxt-sw-5-m" || m.Version != 9 {
		t.Errorf("leader/version = %q/%d, want nxt-sw-5-m/9", m.Leader, m.Version)
	}
	if m.AgeSeconds != 42 {
		t.Errorf("age = %d, want 42 (now − applied_at, on this node's clock)", m.AgeSeconds)
	}
	if m.Settings != 2 || m.Secrets != 1 || m.Users != 1 {
		t.Errorf("counts = %d/%d/%d settings/secrets/users, want 2/1/1", m.Settings, m.Secrets, m.Users)
	}
	if got := m.Human(); got != "from nxt-sw-5-m, version 9, confirmed 42s ago" {
		t.Errorf("Human() = %q", got)
	}
}

// TestConfirmMirrorSyncAgesFromContactNotFromContent is the regression for the bug this
// shipped with: a healthy quiet cluster answers 304 on every poll, applied_at never moves,
// and the mirror warned STALE four minutes in. The fix is that a 304 records a
// CONFIRMATION, which is what an age must be measured from.
func TestConfirmMirrorSyncAgesFromContactNotFromContent(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	applied := int64(1_700_000_000)
	if err := st.ApplySnapshot(ctx, &Snapshot{Version: 3, GeneratedAt: applied}, "nxt-sw-5-m", applied); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}

	// Ten minutes of nothing changing: every poll is a 304, so each one confirms.
	confirmed := applied + 600
	if err := st.ConfirmMirrorSync(ctx, confirmed); err != nil {
		t.Fatalf("ConfirmMirrorSync: %v", err)
	}
	m, err := st.MirrorReading(ctx, time.Unix(confirmed+5, 0))
	if err != nil {
		t.Fatalf("MirrorReading: %v", err)
	}
	if m.AgeSeconds != 5 {
		t.Errorf("age = %ds after a confirmation 5s ago, want 5 — it must not be measured from when the rows last changed", m.AgeSeconds)
	}
	if m.ContentAgeSeconds != 605 {
		t.Errorf("content age = %ds, want 605 (the rows really are that old)", m.ContentAgeSeconds)
	}
	if got := m.ContentAge(); got != "rows last written 10m05s ago" {
		t.Errorf("ContentAge() = %q, want the rows' own age", got)
	}
	// The content age must not be repeated when it says nothing new.
	if small := (&MirrorReading{Leader: "l", Version: 1, AgeSeconds: 10, ContentAgeSeconds: 11}).ContentAge(); small != "" {
		t.Errorf("ContentAge() = %q for content that arrived with the confirmation, want empty", small)
	}
	if stt, err := st.SyncState(ctx); err != nil || stt.AppliedAt != applied {
		t.Errorf("a confirmation moved applied_at (state=%+v err=%v) — it must only move checked_at", stt, err)
	}
}

// TestConfirmMirrorSyncRefusesAnUnsyncedNode: a 304 before any apply is impossible through
// the loop (the ETag comes from this row), so if it ever happens it must not be recorded as
// a successful confirmation.
func TestConfirmMirrorSyncRefusesAnUnsyncedNode(t *testing.T) {
	st := openTestStore(t)
	if err := st.ConfirmMirrorSync(context.Background(), 1_700_000_000); !errors.Is(err, ErrNeverSynced) {
		t.Fatalf("ConfirmMirrorSync on a fresh store = %v, want ErrNeverSynced", err)
	}
}

// TestMirrorReadingClampsANegativeAge: a clock that steps backwards must not print a
// negative age. "applied -12s ago" reads as a bug in the platform, and an operator has no
// way to tell it apart from one.
func TestMirrorReadingClampsANegativeAge(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	applied := int64(1_700_000_000)
	if err := st.ApplySnapshot(ctx, &Snapshot{Version: 1, GeneratedAt: applied}, "nxt-sw-5-m", applied); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}
	m, err := st.MirrorReading(ctx, time.Unix(applied-30, 0))
	if err != nil {
		t.Fatalf("MirrorReading: %v", err)
	}
	if m.AgeSeconds != 0 {
		t.Errorf("age = %d when the local clock is BEHIND applied_at, want 0", m.AgeSeconds)
	}
	if got := m.Human(); got != "from nxt-sw-5-m, version 1, confirmed just now" {
		t.Errorf("Human() = %q", got)
	}
}

// TestMirrorReadingHumanIsCompactButExact: the label is read by a human under pressure, so
// minutes and hours stay readable instead of turning into a float in scientific notation.
func TestMirrorReadingHumanIsCompactButExact(t *testing.T) {
	for _, c := range []struct {
		sec  int64
		want string
	}{
		{0, "just now"}, // rendered without "ago" — see Human
		{59, "59s"},
		{90, "1m30s"},
		{3600, "1h00m"},
		{3661, "1h01m"},
	} {
		m := &MirrorReading{Leader: "l", Version: 1, AgeSeconds: c.sec}
		suffix := " ago"
		if c.sec < 1 {
			suffix = ""
		}
		if got := m.Human(); got != "from l, version 1, confirmed "+c.want+suffix {
			t.Errorf("age %ds → %q, want %q", c.sec, got, c.want)
		}
	}
}
