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

// The feature depends on an App installation token. Enabling it without the App
// is refused by name, and the refusal says why a PAT is not a substitute — an
// operator who reads the error must not reach for the credential that would
// need administration:write on every repo.
func TestEnablingRunnersWithoutAnAppIsRefused(t *testing.T) {
	st := newRunnerSettingsStore(t)
	l := NewLocal(st)

	_, err := l.Update(context.Background(), Settings{
		cluster.SettingGitHubRunnersEnabled(): "true",
		cluster.SettingGitHubRunnerRepos():    "acme/api",
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

func TestEnablingRunnersWithAnAppIsAccepted(t *testing.T) {
	st := newRunnerSettingsStore(t)
	l := NewLocal(st)
	ctx := context.Background()
	if err := st.SetGitHubApp(ctx, &store.GitHubApp{
		AppID: 1, InstallationID: 2, PrivateKeyCiphertext: []byte("not-decrypted-here"),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := l.Update(ctx, Settings{
		cluster.SettingGitHubRunnersEnabled(): "true",
		cluster.SettingGitHubRunnerRepos():    " acme/api , acme/web ,acme/api ",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got[cluster.SettingGitHubRunnersEnabled()] != "true" {
		t.Fatalf("enabled = %q", got[cluster.SettingGitHubRunnersEnabled()])
	}
	if got[cluster.SettingGitHubRunnerRepos()] != "acme/api,acme/web" {
		t.Fatalf("repos = %q, want the normalised comma-separated list", got[cluster.SettingGitHubRunnerRepos()])
	}
}

// Turning the feature OFF must never be gated: an operator clearing the App has
// to be able to stop the runners (and with them the docker-socket exposure).
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

func TestRunnerReposAndMemoryAreValidated(t *testing.T) {
	st := newRunnerSettingsStore(t)
	l := NewLocal(st)
	ctx := context.Background()

	if _, err := l.Update(ctx, Settings{cluster.SettingGitHubRunnerRepos(): "acme"}); err == nil {
		t.Error("a repo without an owner was accepted")
	}
	if _, err := l.Update(ctx, Settings{cluster.SettingGitHubRunnerRepos(): "acme/../etc"}); err == nil {
		t.Error("a path-traversing repo was accepted")
	}
	if _, err := l.Update(ctx, Settings{cluster.SettingGitHubRunnerMemory(): "lots"}); err == nil {
		t.Error("a non-quantity memory limit was accepted")
	}
	if _, err := l.Update(ctx, Settings{cluster.SettingGitHubRunnerMemory(): "0"}); err == nil {
		t.Error("a zero memory limit was accepted")
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
		t.Errorf("enabled default = %q, want false (a docker-socket feature is never on for you)", got[cluster.SettingGitHubRunnersEnabled()])
	}
	if got[cluster.SettingGitHubRunnerMemory()] != cluster.DefaultGitHubRunnerMemory {
		t.Errorf("memory default = %q, want %q", got[cluster.SettingGitHubRunnerMemory()], cluster.DefaultGitHubRunnerMemory)
	}
	if got[cluster.SettingGitHubRunnerAllowLeader()] != "false" {
		t.Errorf("allow-leader default = %q, want false", got[cluster.SettingGitHubRunnerAllowLeader()])
	}
}
