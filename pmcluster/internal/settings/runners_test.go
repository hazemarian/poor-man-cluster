package settings

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func newRunnerSettingsStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func withApp(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.SetGitHubApp(context.Background(), &store.GitHubApp{
		AppID: 1, InstallationID: 2, PrivateKeyCiphertext: []byte("not-decrypted-here"),
	}); err != nil {
		t.Fatal(err)
	}
}

// The feature depends on an App installation token. Enabling it without the App
// is refused by name, and the refusal says why a PAT is not a substitute — an
// operator who reads the error must not reach for the credential that would
// need administration:write on the whole organisation.
func TestEnablingRunnersWithoutAnAppIsRefused(t *testing.T) {
	st := newRunnerSettingsStore(t)
	l := NewLocal(st)

	_, err := l.Update(context.Background(), Settings{
		cluster.SettingGitHubRunnersEnabled(): "true",
		cluster.SettingGitHubRunnerOrg():      "acme",
		cluster.SettingGitHubRunnerNodes():    "worker-1",
	})
	if err == nil {
		t.Fatal("enabling runners without a GitHub App was accepted")
	}
	msg := err.Error()
	for _, want := range []string{"GitHub App", "administration:write", "personal access token"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
	// Nothing was written: the refusal is atomic like every other invalid value.
	got, gerr := l.Get(context.Background())
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got[cluster.SettingGitHubRunnersEnabled()] != "false" {
		t.Fatalf("stored enabled = %q after a refused update", got[cluster.SettingGitHubRunnersEnabled()])
	}
}

// An enabled feature with nowhere to register is a half-enabled state, and the
// refusal names the key to fill in rather than failing later at reconcile time.
func TestEnablingRunnersWithoutAnOrgIsRefused(t *testing.T) {
	st := newRunnerSettingsStore(t)
	withApp(t, st)
	_, err := NewLocal(st).Update(context.Background(), Settings{
		cluster.SettingGitHubRunnersEnabled(): "true",
		cluster.SettingGitHubRunnerNodes():    "worker-1",
	})
	if err == nil {
		t.Fatal("enabling runners with no organisation was accepted")
	}
	if !strings.Contains(err.Error(), cluster.SettingGitHubRunnerOrg()) {
		t.Fatalf("error %q does not name %s", err, cluster.SettingGitHubRunnerOrg())
	}
}

// An enabled feature with no selected node would run nowhere; the picker is the
// fix, and the error says so.
func TestEnablingRunnersWithoutANodeIsRefused(t *testing.T) {
	st := newRunnerSettingsStore(t)
	withApp(t, st)
	_, err := NewLocal(st).Update(context.Background(), Settings{
		cluster.SettingGitHubRunnersEnabled(): "true",
		cluster.SettingGitHubRunnerOrg():      "acme",
	})
	if err == nil {
		t.Fatal("enabling runners with no selected node was accepted")
	}
	if !strings.Contains(err.Error(), cluster.SettingGitHubRunnerNodes()) {
		t.Fatalf("error %q does not name %s", err, cluster.SettingGitHubRunnerNodes())
	}
}

func TestEnablingRunnersWithAppOrgAndNodesIsAccepted(t *testing.T) {
	st := newRunnerSettingsStore(t)
	withApp(t, st)
	l := NewLocal(st)
	ctx := context.Background()
	got, err := l.Update(ctx, Settings{
		cluster.SettingGitHubRunnersEnabled(): "true",
		cluster.SettingGitHubRunnerOrg():      " acme ",
		cluster.SettingGitHubRunnerGroup():    " pmcluster ",
		cluster.SettingGitHubRunnerNodes():    " worker-2 , worker-1 ,worker-2 ",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got[cluster.SettingGitHubRunnersEnabled()] != "true" {
		t.Fatalf("enabled = %q", got[cluster.SettingGitHubRunnersEnabled()])
	}
	if got[cluster.SettingGitHubRunnerOrg()] != "acme" {
		t.Fatalf("org = %q", got[cluster.SettingGitHubRunnerOrg()])
	}
	if got[cluster.SettingGitHubRunnerGroup()] != "pmcluster" {
		t.Fatalf("group = %q", got[cluster.SettingGitHubRunnerGroup()])
	}
	// Ordering is the operator's: the picker writes the list in the order shown,
	// and the setting keeps it (deduped).
	if got[cluster.SettingGitHubRunnerNodes()] != "worker-2,worker-1" {
		t.Fatalf("nodes = %q, want the normalised, order-preserving list", got[cluster.SettingGitHubRunnerNodes()])
	}
}

// Clearing the org while the feature is on is the same half-enabled state as
// enabling without one, so it is refused even though the flag itself is not in
// the update.
func TestClearingTheOrgWhileEnabledIsRefused(t *testing.T) {
	st := newRunnerSettingsStore(t)
	withApp(t, st)
	l := NewLocal(st)
	ctx := context.Background()
	if _, err := l.Update(ctx, Settings{
		cluster.SettingGitHubRunnersEnabled(): "true",
		cluster.SettingGitHubRunnerOrg():      "acme",
		cluster.SettingGitHubRunnerNodes():    "worker-1",
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := l.Update(ctx, Settings{cluster.SettingGitHubRunnerOrg(): ""}); err == nil {
		t.Fatal("clearing github_runner_org while runners are enabled was accepted")
	}
}

// Turning the feature OFF must never be gated: an operator clearing the App has
// to be able to stop the runners (and with them the docker access).
func TestDisablingRunnersNeedsNoApp(t *testing.T) {
	st := newRunnerSettingsStore(t)
	l := NewLocal(st)
	got, err := l.Update(context.Background(), Settings{
		cluster.SettingGitHubRunnersEnabled(): "false",
	})
	if err != nil {
		t.Fatalf("disabling was refused: %v", err)
	}
	if got[cluster.SettingGitHubRunnersEnabled()] != "false" {
		t.Fatalf("enabled = %q", got[cluster.SettingGitHubRunnersEnabled()])
	}
}

func TestRunnerValuesAreValidated(t *testing.T) {
	st := newRunnerSettingsStore(t)
	l := NewLocal(st)
	ctx := context.Background()

	bad := []struct {
		key, value string
		why        string
	}{
		{cluster.SettingGitHubRunnerOrg(), "acme/api", "a repo path is not an organisation"},
		{cluster.SettingGitHubRunnerOrg(), "-acme", "a leading hyphen is not a GitHub login"},
		{cluster.SettingGitHubRunnerGroup(), "a/b", "a slash is not valid in a group name"},
		{cluster.SettingGitHubRunnerNodes(), "worker 1", "a space is not a hostname"},
		{cluster.SettingGitHubRunnerMemory(), "lots", "a non-quantity memory limit"},
		{cluster.SettingGitHubRunnerMemory(), "0", "a zero memory limit"},
	}
	for _, c := range bad {
		if _, err := l.Update(ctx, Settings{c.key: c.value}); err == nil {
			t.Errorf("%s=%q was accepted (%s)", c.key, c.value, c.why)
		}
	}
	got, err := l.Update(ctx, Settings{cluster.SettingGitHubRunnerMemory(): "4g"})
	if err != nil {
		t.Fatalf("4g refused: %v", err)
	}
	if got[cluster.SettingGitHubRunnerMemory()] != "4g" {
		t.Fatalf("memory = %q", got[cluster.SettingGitHubRunnerMemory()])
	}
}

// The console and the CLI must show the value actually in effect, so the
// defaults are reported rather than an empty string.
func TestRunnerSettingDefaultsAreReported(t *testing.T) {
	st := newRunnerSettingsStore(t)
	got, err := NewLocal(st).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got[cluster.SettingGitHubRunnersEnabled()] != "false" {
		t.Errorf("enabled default = %q, want false (a docker-access feature is never on for you)", got[cluster.SettingGitHubRunnersEnabled()])
	}
	if got[cluster.SettingGitHubRunnerGroup()] != cluster.DefaultGitHubRunnerGroup {
		t.Errorf("group default = %q, want %q", got[cluster.SettingGitHubRunnerGroup()], cluster.DefaultGitHubRunnerGroup)
	}
	// There is no docker-access setting any more: a job always gets its own daemon,
	// so a key that says otherwise must not exist. Asserting its absence is the point
	// — a resurrected key would be a control that changes nothing.
	for _, gone := range []string{"github_runner_socket_proxy", "github_runner_socket_path", "github_runner_dind"} {
		if _, ok := got[gone]; ok {
			t.Errorf("%q is back in the settings surface; a job's docker access is not a choice", gone)
		}
	}
	if got[cluster.SettingGitHubRunnerMemory()] != cluster.DefaultGitHubRunnerMemory {
		t.Errorf("memory default = %q, want %q", got[cluster.SettingGitHubRunnerMemory()], cluster.DefaultGitHubRunnerMemory)
	}
	if got[cluster.SettingGitHubRunnerAllowManagers()] != "false" {
		t.Errorf("allow-managers default = %q, want false", got[cluster.SettingGitHubRunnerAllowManagers()])
	}
}
