package upgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fastPolls keeps the health gate from spending three seconds per poll in tests.
func fastPolls(t *testing.T) {
	t.Helper()
	prev := pollInterval
	pollInterval = time.Millisecond
	t.Cleanup(func() { pollInterval = prev })
}

func tg(host, role string, leader bool) Target {
	return Target{Host: host, User: "root", Role: role, Leader: leader}
}

// TestPlan_OrdersWorkersThenManagersThenTheLeader is the rule the old script documented and the
// one that keeps a rollout from restarting the leader mid-sequence: the leader goes last, after
// every other node can answer.
func TestPlan_OrdersWorkersThenManagersThenTheLeader(t *testing.T) {
	got, err := Plan([]Target{
		tg("leader", "manager", true),
		tg("m1", "manager", false),
		tg("w1", "worker", false),
		tg("w2", "worker", false),
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := []string{"w1", "w2", "m1", "leader"}
	for i, w := range want {
		if got[i].Host != w {
			t.Fatalf("order = %v, want %v", hosts(got), want)
		}
	}
}

// TestPlan_RefusesWithoutALeader: a rollout that cannot name the leader will guess, and guessing
// wrong restarts the leader in the middle. The old script's ordering silently degraded to the
// typed order when the probe matched nothing; refusing is the whole improvement.
func TestPlan_RefusesWithoutALeader(t *testing.T) {
	_, err := Plan([]Target{tg("m1", "manager", false), tg("m2", "manager", false)})
	if err == nil {
		t.Fatal("Plan ordered a rollout with no leader")
	}
	if !strings.Contains(err.Error(), "no node claims leadership") {
		t.Fatalf("unhelpful refusal: %v", err)
	}
}

func TestPlan_RefusesTwoLeaders(t *testing.T) {
	if _, err := Plan([]Target{tg("a", "manager", true), tg("b", "manager", true)}); err == nil {
		t.Fatal("Plan accepted two leaders; \"the leader last\" has no meaning there")
	}
}

func TestPlan_RefusesEmpty(t *testing.T) {
	if _, err := Plan(nil); err == nil {
		t.Fatal("Plan accepted an empty target list")
	}
}

// fakeTransport scripts node behaviour: version before, version after install, and whether
// /health ever answers with the new version.
type fakeTransport struct {
	mu sync.Mutex
	// versions maps host -> installed version.
	versions map[string]string
	// healthy maps host -> whether /health answers 200.
	healthy map[string]bool
	// installFails maps host -> the install script fails.
	installFails map[string]bool
	// rollbackFails makes the rollback script fail.
	rollbackFails bool
	// calls records every script run, per host.
	calls []string
}

func newFake(versions map[string]string) *fakeTransport {
	f := &fakeTransport{versions: map[string]string{}, healthy: map[string]bool{}, installFails: map[string]bool{}}
	for h, v := range versions {
		f.versions[h] = v
		f.healthy[h] = true
	}
	return f
}

func (f *fakeTransport) Run(_ context.Context, t Target, script string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(script, " version"):
		// Exactly what the binary prints: the banner is part of the answer.
		return "pmcluster " + f.versions[t.Host], nil
	case strings.Contains(script, "sha256sum -c"):
		if f.installFails[t.Host] {
			return "", errors.New("sha256sum: FAILED")
		}
		// The install script ends by restarting: the version becomes the one it installed.
		// The fake reads it from the script's own artifact name.
		if v := versionFromScript(script); v != "" {
			f.versions[t.Host] = v
		}
		return "", nil
	case strings.Contains(script, "curl -fsS"):
		if !f.healthy[t.Host] {
			return "", errors.New("curl: connection refused")
		}
		return `{"status":"ok","version":"` + f.versions[t.Host] + `"}`, nil
	case strings.Contains(script, prevPath) && strings.Contains(script, "install"):
		if f.rollbackFails {
			return "", errors.New("no previous binary")
		}
		// The previous binary is the one the node was happily running before the rollout:
		// restoring it puts the node back in service, which is what the re-gate must see.
		f.healthy[t.Host] = true
		return "", nil
	}
	f.calls = append(f.calls, t.Host+": "+script)
	return "", nil
}

// Put models the copy: recorded so a test can assert bytes actually went to a node.
func (f *fakeTransport) Put(_ context.Context, t Target, localPath, remotePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "put "+t.Host+": "+remotePath)
	return nil
}

// versionFromScript pulls "v0.0.0" out of the artifact URL in the install script.
func versionFromScript(script string) string {
	i := strings.Index(script, "-linux-amd64.tar.gz")
	if i < 0 {
		return ""
	}
	// Asset names look like pmcluster-v0.2.347-linux-amd64.tar.gz; the tag is between
	// "pmcluster-" and "-linux".
	line := script[:i]
	j := strings.LastIndex(line, "pmcluster-")
	if j < 0 {
		return ""
	}
	return line[j+len("pmcluster-"):]
}

func hosts(ts []Target) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Host)
	}
	return out
}

// writeFakeArtifact stands in for the downloaded tarball: Run only stat's it, the transport
// copies it, and the fake records the copy.
func writeFakeArtifact(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pmcluster-v0.2.347-linux-amd64.tar.gz")
	if err := os.WriteFile(p, []byte("fake tarball"), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	return p
}

func opts(t *testing.T, f *fakeTransport, targets ...Target) Options {
	return Options{
		Version:       "v0.2.347",
		Artifact:      writeFakeArtifact(t),
		SHA256:        strings.Repeat("a", 64),
		Targets:       targets,
		Transport:     f,
		HealthTimeout: 50 * 1000 * 1000, // 50ms: the fake answers on the first poll
	}
}

// TestRun_HappyPathUpgradesInOrderAndVerifies: the rollback copy is taken, the checksum is
// verified before the install, and the node is only accepted when /health reports the NEW
// version.
func TestRun_HappyPathUpgradesInOrderAndVerifies(t *testing.T) {
	fastPolls(t)
	f := newFake(map[string]string{"w1": "v0.2.346", "leader": "v0.2.346"})
	o := opts(t, f, tg("leader", "manager", true), tg("w1", "worker", false))
	var log strings.Builder
	o.Logf = func(format string, args ...any) { log.WriteString(fmt.Sprintf(format, args...) + "\n") }

	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Failed() || rep.Aborted {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.Nodes) != 2 || rep.Nodes[0].Target.Host != "w1" || rep.Nodes[1].Target.Host != "leader" {
		t.Fatalf("nodes = %+v, want w1 then leader", rep.Nodes)
	}
	for _, n := range rep.Nodes {
		if n.From != "v0.2.346" || n.To != "v0.2.347" || n.Skipped {
			t.Errorf("%s: %+v, want v0.2.346 → v0.2.347", n.Target.Host, n)
		}
	}
	if !strings.Contains(log.String(), "✔ healthy on v0.2.347") {
		t.Errorf("the gate did not report a verified node:\n%s", log.String())
	}
}

// TestRun_UnhealthyNodeIsRolledBackAndTheRolloutStops is the failure the script could not see:
// the daemon restarts and /health never reports the new version. The node must be restored and
// the remaining nodes left ALONE — a half-upgraded fleet with one broken node is recoverable, a
// fleet that carried on past it is how mixed versions and a dead leader happen.
func TestRun_UnhealthyNodeIsRolledBackAndTheRolloutStops(t *testing.T) {
	fastPolls(t)
	f := newFake(map[string]string{"w1": "v0.2.346", "w2": "v0.2.346", "leader": "v0.2.346"})
	f.healthy["w1"] = false // the new binary never answers /health
	o := opts(t, f, tg("leader", "manager", true), tg("w1", "worker", false), tg("w2", "worker", false))
	var log strings.Builder
	o.Logf = func(format string, args ...any) { log.WriteString(fmt.Sprintf(format, args...) + "\n") }

	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run returned a transport error instead of a report: %v", err)
	}
	if !rep.Aborted {
		t.Fatal("the rollout continued past a node that never came up healthy")
	}
	if !rep.Failed() || len(rep.Nodes) != 1 {
		t.Fatalf("report = %+v, want exactly one failed node", rep.Nodes)
	}
	first := rep.Nodes[0]
	if first.Target.Host != "w1" || !strings.Contains(first.Err, "health gate") {
		t.Fatalf("first node = %+v", first)
	}
	// The rollback ran, was VERIFIED, and the node is reported back on its old version.
	if !strings.Contains(log.String(), "rolling back") {
		t.Errorf("no rollback attempt:\n%s", log.String())
	}
	if !first.RolledBack || first.To != "v0.2.346" {
		t.Errorf("rollback was not recorded as verified: %+v", first)
	}
	if strings.Contains(first.Err, "rollback FAILED") {
		t.Errorf("the rollback was reported as failed although the fake restored the node: %q", first.Err)
	}
	// w2 and the leader were never touched: no install script reached them.
	for _, c := range f.calls {
		if strings.HasPrefix(c, "w2:") || strings.HasPrefix(c, "leader:") {
			t.Errorf("a node after the failure was touched: %s", c)
		}
	}
}

// TestRun_FailedRollbackIsReportedLoudly: restoring the file and restarting can leave the node
// exactly as broken as the release it replaced. Saying "rolled back" there would be a lie.
func TestRun_FailedRollbackIsReportedLoudly(t *testing.T) {
	fastPolls(t)
	f := newFake(map[string]string{"w1": "v0.2.346", "leader": "v0.2.346"})
	f.healthy["w1"] = false
	f.rollbackFails = true
	o := opts(t, f, tg("leader", "manager", true), tg("w1", "worker", false))

	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Aborted || len(rep.Nodes) != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if !strings.Contains(rep.Nodes[0].Err, "rollback FAILED") {
		t.Fatalf("a failed rollback was not reported as such: %q", rep.Nodes[0].Err)
	}
	if rep.Nodes[0].RolledBack {
		t.Fatal("RolledBack is true for a rollback that failed")
	}
}

// TestRun_SkipsANodeAlreadyOnTheVersion: the rollout resumes instead of reinstalling, which is
// what makes "the third node failed, fix it and run again" cheap.
func TestRun_SkipsANodeAlreadyOnTheVersion(t *testing.T) {
	fastPolls(t)
	f := newFake(map[string]string{"w1": "v0.2.347", "leader": "v0.2.346"})
	o := opts(t, f, tg("leader", "manager", true), tg("w1", "worker", false))

	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Nodes) != 2 || !rep.Nodes[0].Skipped {
		t.Fatalf("nodes = %+v, want w1 skipped", rep.Nodes)
	}
	if rep.Nodes[1].Skipped || rep.Nodes[1].To != "v0.2.347" {
		t.Fatalf("leader = %+v, want upgraded", rep.Nodes[1])
	}
}

// TestRun_SkipsWhenTheBannerCarriesTheVersion pins the comparison: `pmcluster version` prints
// "pmcluster v0.2.346", and a rollout that compares the whole line decides every node is out of
// date — which is what happened on a real fleet (it reinstalled the version already running).
func TestRun_SkipsWhenTheBannerCarriesTheVersion(t *testing.T) {
	fastPolls(t)
	f := newFake(map[string]string{"w1": "v0.2.347", "leader": "v0.2.347"})
	o := opts(t, f, tg("leader", "manager", true), tg("w1", "worker", false))

	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, n := range rep.Nodes {
		if !n.Skipped {
			t.Fatalf("%s was reinstalled at the version it already ran: %+v", n.Target.Host, n)
		}
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "put ") {
			t.Fatalf("a node already on the version received the artifact: %s", c)
		}
	}
}

// TestRun_RefusesWithoutAVerifiableArtifact: an unverified binary is not an upgrade.
func TestRun_RefusesWithoutAVerifiableArtifact(t *testing.T) {
	f := newFake(map[string]string{"w1": "v0.2.346"})
	o := opts(t, f, tg("w1", "worker", false))
	o.SHA256 = ""
	if _, err := Run(context.Background(), o); err == nil {
		t.Fatal("Run installed a binary with no checksum")
	}
}
