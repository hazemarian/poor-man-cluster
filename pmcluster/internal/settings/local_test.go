package settings

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func TestLocalGetUpdateRoundTrip(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := NewLocal(st)
	ctx := context.Background()

	// Get on a fresh store returns all allowlisted keys empty.
	got, err := l.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(clusterSettingKeys) {
		t.Fatalf("Get returned %d keys, want %d", len(got), len(clusterSettingKeys))
	}
	for _, k := range clusterSettingKeys {
		if _, ok := got[k]; !ok {
			t.Errorf("Get missing key %s", k)
		}
	}

	// Update persists and Get reflects it.
	updated, err := l.Update(ctx, Settings{clusterSettingKeys[0]: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if updated[clusterSettingKeys[0]] != "abc" {
		t.Errorf("Update returned %q, want abc", updated[clusterSettingKeys[0]])
	}
	got, _ = l.Get(ctx)
	if got[clusterSettingKeys[0]] != "abc" {
		t.Errorf("Get after Update = %q, want abc", got[clusterSettingKeys[0]])
	}
}

func TestLocalUpdateRejectsUnknownKey(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := NewLocal(st)

	if _, err := l.Update(context.Background(), Settings{"not_a_real_key": "x"}); err == nil {
		t.Fatal("Update with unknown key: want error, got nil")
	}
}

func TestLocalUpdateRequiresNonEmpty(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := NewLocal(st)

	if _, err := l.Update(context.Background(), Settings{}); err == nil {
		t.Fatal("Update with empty settings: want error, got nil")
	}
}

func TestLocalUpdateAppliesLogLevel(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var applied string
	l := NewLocal(st)
	l.ApplyLogLevel = func(level string) error { applied = level; return nil }
	ctx := context.Background()

	if _, err := l.Update(ctx, Settings{cluster.SettingLogLevel(): "debug"}); err != nil {
		t.Fatal(err)
	}
	if applied != "debug" {
		t.Errorf("ApplyLogLevel called with %q, want debug", applied)
	}

	// A non-log_level key must not trigger the hook.
	applied = ""
	if _, err := l.Update(ctx, Settings{cluster.SettingDomain(): "example.com"}); err != nil {
		t.Fatal(err)
	}
	if applied != "" {
		t.Errorf("ApplyLogLevel triggered by non-log_level key: %q", applied)
	}
}
