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

// retentionKeys is the three per-stream retention settings, written out so a
// table can walk all of them instead of testing one and hoping.
var retentionKeys = []string{
	cluster.SettingObservabilityLogsRetentionDays(),
	cluster.SettingObservabilityMetricsRetentionDays(),
	cluster.SettingObservabilityTracesRetentionDays(),
}

func newLocalTestStore(t *testing.T) (*Local, func(context.Context, string) string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewLocal(st), func(ctx context.Context, key string) string {
		return st.SettingDefault(ctx, key, "")
	}
}

// TestLocalRetentionAcceptsWholeDaysAndGetReportsIt: each of the three keys
// accepts a whole number of days, stores the canonical form (whitespace and
// leading zeros removed) and `Get` — not just the row — reports it, which is
// what the console and the provision renderer actually read.
func TestLocalRetentionAcceptsWholeDaysAndGetReportsIt(t *testing.T) {
	ctx := context.Background()
	want := map[string]string{
		cluster.SettingObservabilityLogsRetentionDays():    "30",
		cluster.SettingObservabilityMetricsRetentionDays(): "14",
		cluster.SettingObservabilityTracesRetentionDays():  "10",
	}

	for _, key := range retentionKeys {
		for _, in := range []struct{ raw, stored string }{
			{raw: "1", stored: "1"},
			{raw: "7", stored: "7"},
			{raw: " 21 ", stored: "21"},
			{raw: "365", stored: "365"},
		} {
			t.Run(key+"="+in.raw, func(t *testing.T) {
				l, _ := newLocalTestStore(t)
				updated, err := l.Update(ctx, Settings{key: in.raw})
				if err != nil {
					t.Fatalf("Update(%q): %v", in.raw, err)
				}
				if updated[key] != in.stored {
					t.Errorf("Update returned %q, want %q", updated[key], in.stored)
				}
				got, err := l.Get(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if got[key] != in.stored {
					t.Errorf("Get = %q, want %q (the stored value must override the default)", got[key], in.stored)
				}
				// The other two streams are untouched: a per-stream setting must
				// not bleed into its siblings.
				for other, def := range want {
					if other == key {
						continue
					}
					if got[other] != def {
						t.Errorf("%s = %q after setting %s, want the default %q", other, got[other], key, def)
					}
				}
			})
		}
	}
}

// TestLocalRetentionRejectsInvalidValues: zero, negative, non-numeric and a
// fractional day are refused for every key, with a message naming the accepted
// range, and nothing is persisted (the rejection is atomic).
func TestLocalRetentionRejectsInvalidValues(t *testing.T) {
	for _, key := range retentionKeys {
		for _, bad := range []string{"0", "-1", "-30", "abc", "1.5", "1,5", " ", "", "30d", "1e3"} {
			t.Run(key+"="+bad, func(t *testing.T) {
				ctx := context.Background()
				l, stored := newLocalTestStore(t)
				_, err := l.Update(ctx, Settings{key: bad})
				if err == nil {
					t.Fatalf("Update(%q) accepted an invalid retention", bad)
				}
				if !strings.Contains(err.Error(), "1 or more") {
					t.Errorf("error %q must name the accepted range (1 or more days)", err)
				}
				if !strings.Contains(err.Error(), key) {
					t.Errorf("error %q must name the key", err)
				}
				if v := stored(ctx, key); v != "" {
					t.Errorf("rejected value %q was persisted as %q", bad, v)
				}
			})
		}
	}
}

// TestLocalRetentionRejectionIsAtomicAcrossKeys: one bad key refuses the whole
// update, so a caller cannot half-apply a retention change.
func TestLocalRetentionRejectionIsAtomicAcrossKeys(t *testing.T) {
	ctx := context.Background()
	l, stored := newLocalTestStore(t)

	_, err := l.Update(ctx, Settings{
		cluster.SettingObservabilityLogsRetentionDays():   "21",
		cluster.SettingObservabilityTracesRetentionDays(): "0",
	})
	if err == nil {
		t.Fatal("want an error for the invalid trace retention")
	}
	if v := stored(ctx, cluster.SettingObservabilityLogsRetentionDays()); v != "" {
		t.Fatalf("the valid logs retention was written despite the rejected sibling: %q", v)
	}
}
