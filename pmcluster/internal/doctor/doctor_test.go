package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
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

// stubCredHelper answers the pull-path half without running a command.
type stubCredHelper struct {
	secret string
	err    error
	calls  int
}

func (s *stubCredHelper) GHCRToken(context.Context) (string, error) {
	s.calls++
	return s.secret, s.err
}

// TestGitHubAppCheckProvesThePullPath is the difference between "the key mints"
// and "a pull works": the check must run the node's helper, and a passing check
// must say so.
func TestGitHubAppCheckProvesThePullPath(t *testing.T) {
	helper := &stubCredHelper{secret: "ghs_doctor_test"}
	c := checkGitHubApp(context.Background(), Input{
		GitHubApp:        stubGitHubApp{detail: "minted a token; read acme/app OK", expiry: time.Now().UTC()},
		CredentialHelper: helper,
	})
	if c.Status != StatusOK {
		t.Fatalf("status = %q, want ok (%s)", c.Status, c.Detail)
	}
	if helper.calls != 1 {
		t.Errorf("the credential helper ran %d times, want once — the check must exercise the pull path", helper.calls)
	}
	if !strings.Contains(c.Detail, "docker-credential-pmcluster get ghcr.io returned a token") {
		t.Errorf("detail %q does not report the helper's answer", c.Detail)
	}
	if strings.Contains(c.Detail, "ghs_") {
		t.Errorf("detail %q leaked the token", c.Detail)
	}
}

// TestGitHubAppCheckFailsWhenTheHelperCannotFetch: a mint that works but a pull
// that cannot is a failing check, and the sentence names both ends of the path.
func TestGitHubAppCheckFailsWhenTheHelperCannotFetch(t *testing.T) {
	c := checkGitHubApp(context.Background(), Input{
		GitHubApp: stubGitHubApp{detail: "minted a token", expiry: time.Now().UTC()},
		CredentialHelper: &stubCredHelper{
			err: errors.New("reach the credential listener at https://127.0.0.1:9443: connection refused"),
		},
	})
	if c.Status != StatusFail {
		t.Fatalf("status = %q, want fail", c.Status)
	}
	if !strings.Contains(c.Detail, "docker-credential-pmcluster") || !strings.Contains(c.Detail, "pmcluster-edge") {
		t.Errorf("detail %q does not name the helper and the edge", c.Detail)
	}
	if !strings.Contains(c.Detail, "connection refused") {
		t.Errorf("detail %q does not carry the cause", c.Detail)
	}
}

// TestGitHubAppCheckUnconfiguredNamesTheFixAndSkipsTheHelper: with no app there
// is nothing to mint, so the check skips and says which command configures one —
// it must not fail on the helper, whose failure would only restate the cause.
func TestGitHubAppCheckUnconfiguredNamesTheFixAndSkipsTheHelper(t *testing.T) {
	helper := &stubCredHelper{secret: "ghs_never"}
	c := checkGitHubApp(context.Background(), Input{
		GitHubApp:        stubGitHubApp{err: githubapp.ErrNoApp},
		CredentialHelper: helper,
	})
	if c.Status != StatusSkip {
		t.Fatalf("status = %q, want skip", c.Status)
	}
	if !strings.Contains(c.Detail, "pmcluster github app set") {
		t.Errorf("detail %q does not name the command that fixes it", c.Detail)
	}
	if helper.calls != 0 {
		t.Error("the credential helper ran with no app configured")
	}
}
