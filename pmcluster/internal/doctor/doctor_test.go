package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// doctor must be honest about what it could not measure: a host without a
// swarm, a store or a backup state dir reports skipped checks with reasons, and
// "nothing failed" must not mean "nothing was checked".
func TestRunSkipsWhatItCannotMeasure(t *testing.T) {
	rep, err := Run(context.Background(), Input{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Checks) == 0 {
		t.Fatal("no checks reported")
	}
	for _, c := range rep.Checks {
		if c.Status == "" {
			t.Errorf("check %q has no status", c.Name)
		}
		if c.Status == StatusSkip && c.Detail == "" {
			t.Errorf("check %q is skipped without a reason", c.Name)
		}
	}
	if !rep.OK {
		t.Error("a host with nothing to check must not report failure")
	}
}

// A restore drill that has never run is a warning, not a pass: the whole point
// of the check is that "we have not tried" is not "we are fine".
func TestRestoreDrillIsAWarningWhenNoDrillHasRun(t *testing.T) {
	c := checkDrill(Input{BackupStateDir: t.TempDir()})
	if c.Status != StatusWarn {
		t.Fatalf("status = %q, want warn (%s)", c.Status, c.Detail)
	}
}

// A failed drill is a failure, and the detail names when it failed.
func TestRestoreDrillReportsTheLastFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "drill"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{"last":{"at":"` + time.Now().UTC().Format(time.RFC3339) + `","archive":"a.tar.gz","ok":false,"error":"sqlite locked"}}`
	if err := os.WriteFile(filepath.Join(dir, "drill", "status.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := checkDrill(Input{BackupStateDir: dir}); c.Status != StatusFail {
		t.Fatalf("status = %q, want fail (%s)", c.Status, c.Detail)
	}
}

// The offsite check is the one that catches a bucket that expired its objects,
// so a recorded miss has to fail the doctor — not warn.
func TestOffsiteMissIsAFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "offsite"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{"last":{"at":"` + time.Now().UTC().Format(time.RFC3339) + `","target":"s3:b","checked":10,"present":8,"missing":["a.tar.gz","b.tar.gz"],"ok":false}}`
	if err := os.WriteFile(filepath.Join(dir, "offsite", "status.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c := checkOffsite(Input{BackupStateDir: dir})
	if c.Status != StatusFail {
		t.Fatalf("status = %q, want fail (%s)", c.Status, c.Detail)
	}
}

// A worker cannot read the manager set; it must say so rather than report a
// healthy swarm it never inspected.
func TestSwarmIsSkippedOnAWorker(t *testing.T) {
	c := checkSwarm(context.Background(), Input{Docker: workerInfo{}})
	if c.Status != StatusSkip {
		t.Fatalf("status = %q, want skip (%s)", c.Status, c.Detail)
	}
}

// workerInfo reports a swarm the node does not control.
type workerInfo struct{ runtime.Client }

func (w workerInfo) Info(context.Context) (runtime.Info, error) {
	return runtime.Info{SwarmControlAvailable: false}, nil
}

// nodeListDocker answers NodeList only; every other call panics via the nil
// embedded interface, which is what a check that reaches past its remit should
// do loudly.
type nodeListDocker struct {
	runtime.Client
	nodes []runtime.Node
}

func (d nodeListDocker) NodeList(context.Context) ([]runtime.Node, error) { return d.nodes, nil }

// An empty-valued pmcluster.* label is not "no label": the key is still there,
// so anything matching on presence reads the node as labelled. That is how
// nxt-sw-3-w carried `pmcluster.storage: ""` and still looked like storage.
func TestNodeLabelsReportsAnEmptyValuedLabel(t *testing.T) {
	c := checkNodeLabels(context.Background(), Input{Docker: nodeListDocker{nodes: []runtime.Node{
		{Hostname: "nxt-sw-3-w", Role: "worker", Labels: map[string]string{runtime.StorageNodeLabel: ""}},
		{Hostname: "nxt-sw-4-m", Role: "manager", Labels: map[string]string{runtime.StorageNodeLabel: "true"}},
	}}})
	if c.Status != StatusWarn {
		t.Fatalf("status = %q, want warn (%s)", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "nxt-sw-3-w") || !strings.Contains(c.Detail, runtime.StorageNodeLabel) {
		t.Fatalf("detail must name the node and the label, got %q", c.Detail)
	}
}

func TestNodeLabelsIsOKWhenEveryLabelHasAValue(t *testing.T) {
	c := checkNodeLabels(context.Background(), Input{Docker: nodeListDocker{nodes: []runtime.Node{
		{Hostname: "nxt-sw-4-m", Role: "manager", Labels: map[string]string{runtime.StorageNodeLabel: "true"}},
	}}})
	if c.Status != StatusOK {
		t.Fatalf("status = %q, want ok (%s)", c.Status, c.Detail)
	}
}

// stubGitHubApp answers the GitHub App check without a network.
type stubGitHubApp struct {
	detail string
	expiry time.Time
	err    error
}

func (s stubGitHubApp) Diagnose(context.Context) (string, time.Time, error) {
	return s.detail, s.expiry, s.err
}

// The GitHub App check must distinguish the four causes: no app is a skip (not
// a failure), while a rejected key, a removed installation and a missing
// permission are failures with different sentences.
func TestGitHubAppCheckDistinguishesTheCauses(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name   string
		in     Input
		status Status
		text   string
	}{
		{"no probe wired", Input{}, StatusSkip, "no store/cipher"},
		{"no app", Input{GitHubApp: stubGitHubApp{err: githubapp.ErrNoApp}}, StatusSkip, "no GitHub App configured"},
		{"ok", Input{GitHubApp: stubGitHubApp{detail: "minted a token; read acme/app OK", expiry: now}}, StatusOK, "acme/app"},
		{"no probe target", Input{GitHubApp: stubGitHubApp{err: githubapp.ErrNoProbeTarget, expiry: now}}, StatusWarn, "no probe target"},
		{"key rejected", Input{GitHubApp: stubGitHubApp{err: githubapp.ErrBadKey}}, StatusFail, "key/id was rejected"},
		{"installation removed", Input{GitHubApp: stubGitHubApp{err: githubapp.ErrInstallationNotFound}}, StatusFail, "installation is gone"},
		{"insufficient permission", Input{GitHubApp: stubGitHubApp{err: githubapp.ErrInsufficientPermission}}, StatusFail, "insufficient permission"},
		{"token rejected", Input{GitHubApp: stubGitHubApp{err: githubapp.ErrTokenRejected}}, StatusFail, "token was rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := checkGitHubApp(context.Background(), tc.in)
			if c.Name != "github app" {
				t.Errorf("check name = %q", c.Name)
			}
			if c.Status != tc.status {
				t.Errorf("status = %q, want %q (%s)", c.Status, tc.status, c.Detail)
			}
			if !strings.Contains(c.Detail, tc.text) {
				t.Errorf("detail %q does not mention %q", c.Detail, tc.text)
			}
		})
	}
}

// The pull credential is a read-only PAT that expires 90 days after creation.
// doctor's job is to say so at 60 days — a warning, never a failure, because an
// ageing credential still authenticates pulls.
func TestRegistryCredentialWarnsAt60DaysAndNamesRotation(t *testing.T) {
	st := doctorStore(t)
	ctx := context.Background()
	if err := st.CreateRegistry(ctx, &store.Registry{
		Host: "ghcr.io", Username: "pull-bot", PasswordCiphertext: []byte("ciphertext"),
	}); err != nil {
		t.Fatalf("CreateRegistry: %v", err)
	}
	row, err := st.Registry(ctx, "ghcr.io")
	if err != nil {
		t.Fatalf("Registry: %v", err)
	}
	created := time.Unix(row.CreatedAt, 0)

	check := func(now time.Time) (Check, bool) {
		t.Helper()
		rep, err := Run(ctx, Input{Store: st, Now: now})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		for _, c := range rep.Checks {
			if c.Name == "registry credential" {
				return c, rep.OK
			}
		}
		t.Fatal("Run reported no `registry credential` check")
		return Check{}, false
	}

	if got, _ := check(created.Add(10 * 24 * time.Hour)); got.Status != StatusOK {
		t.Errorf("10-day-old credential = %s (%s), want ok", got.Status, got.Detail)
	}
	fresh, _ := check(created.Add(59 * 24 * time.Hour))
	if fresh.Status != StatusOK {
		t.Errorf("59-day-old credential = %s, want ok", fresh.Status)
	}
	old, ok := check(created.Add(61 * 24 * time.Hour))
	if old.Status != StatusWarn {
		t.Fatalf("61-day-old credential = %s, want warn", old.Status)
	}
	if !ok {
		t.Error("an ageing credential must warn, not make doctor fail")
	}
	for _, want := range []string{"61 days old", "expire at 90 days", "rotate with", "pmcluster registry add ghcr.io"} {
		if !strings.Contains(old.Detail, want) {
			t.Errorf("warning does not carry %q: %q", want, old.Detail)
		}
	}
}

// A cluster with no stored pull credential cannot pull private images, but
// that is a skip with the reason and the fix — not a failure of every other
// check.
func TestRegistryCredentialWithoutARowIsASkip(t *testing.T) {
	st := doctorStore(t)
	rep, err := Run(context.Background(), Input{Store: st})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, c := range rep.Checks {
		if c.Name != "registry credential" {
			continue
		}
		if c.Status != StatusSkip {
			t.Fatalf("missing credential = %s, want skip", c.Status)
		}
		if !strings.Contains(c.Detail, "pmcluster registry add ghcr.io") {
			t.Errorf("skip does not name the fix: %q", c.Detail)
		}
		return
	}
	t.Fatal("no `registry credential` check")
}

func doctorStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
