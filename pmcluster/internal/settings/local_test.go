package settings

import (
	"context"
	"path/filepath"
	"strings"
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

// TestLocalGetReportsLogLevelDefaults: both level settings report the value
// actually in effect, so "unset" is never mistaken for "no logging policy".
func TestLocalGetReportsLogLevelDefaults(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	got, err := NewLocal(st).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got[cluster.SettingLogLevel()] != "info" {
		t.Errorf("log_level = %q, want info (the daemon default)", got[cluster.SettingLogLevel()])
	}
	if got[cluster.SettingAppLogLevel()] != "warn" {
		t.Errorf("app_log_level = %q, want warn (the app default)", got[cluster.SettingAppLogLevel()])
	}
}

// TestLocalUpdateValidatesLevels: an unknown level is refused BEFORE anything
// is written, and the warning alias is stored as the canonical name.
func TestLocalUpdateValidatesLevels(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := NewLocal(st)
	ctx := context.Background()

	_, err = l.Update(ctx, Settings{
		cluster.SettingAppLogLevel(): "verbose",
		cluster.SettingDomain():      "example.com",
	})
	if err == nil {
		t.Fatal("Update with an unknown app_log_level: want error, got nil")
	}
	if !strings.Contains(err.Error(), "debug|info|warn|error") {
		t.Errorf("error %q should name the allowed set", err.Error())
	}
	// Atomic: the valid key in the same request must not have been written.
	if v := st.SettingDefault(ctx, cluster.SettingDomain(), ""); v != "" {
		t.Errorf("domain was written despite the rejected app_log_level: %q", v)
	}

	if _, err := l.Update(ctx, Settings{cluster.SettingAppLogLevel(): "Warning"}); err != nil {
		t.Fatalf("Update with the warning alias: %v", err)
	}
	if v := st.SettingDefault(ctx, cluster.SettingAppLogLevel(), ""); v != "warn" {
		t.Errorf("app_log_level stored as %q, want warn (normalized)", v)
	}
}

// TestLocalGetReportsRetentionDefaults: the three retention keys report the
// per-stream default actually in effect, so "unset" is never mistaken for
// "unbounded".
func TestLocalGetReportsRetentionDefaults(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	got, err := NewLocal(st).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		cluster.SettingObservabilityLogsRetentionDays():    "30",
		cluster.SettingObservabilityMetricsRetentionDays(): "14",
		cluster.SettingObservabilityTracesRetentionDays():  "10",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}

// TestLocalUpdateValidatesRetention: a retention value that is not a whole
// number of days >= 1 is refused atomically with a message naming the range.
func TestLocalUpdateValidatesRetention(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := NewLocal(st)
	ctx := context.Background()

	for _, bad := range []string{"0", "-1", "abc", "1.5", "  "} {
		_, err := l.Update(ctx, Settings{cluster.SettingObservabilityLogsRetentionDays(): bad})
		if err == nil {
			t.Fatalf("Update with retention %q: want error, got nil", bad)
		}
		if !strings.Contains(err.Error(), "1 or more") {
			t.Errorf("error %q should name the allowed range", err.Error())
		}
		// Atomic: nothing may be persisted for a rejected value.
		if v := st.SettingDefault(ctx, cluster.SettingObservabilityLogsRetentionDays(), ""); v != "" {
			t.Errorf("rejected retention %q was still written: %q", bad, v)
		}
	}

	// A valid value is accepted and stored as the canonical number.
	if _, err := l.Update(ctx, Settings{cluster.SettingObservabilityMetricsRetentionDays(): " 21 "}); err != nil {
		t.Fatalf("Update with a valid retention: %v", err)
	}
	if v := st.SettingDefault(ctx, cluster.SettingObservabilityMetricsRetentionDays(), ""); v != "21" {
		t.Errorf("metrics retention stored as %q, want 21", v)
	}
}
