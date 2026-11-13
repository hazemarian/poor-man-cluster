package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dockerHarness is one node's docker check: a probe the test controls, a unit state
// the test controls, and counters for what the check decided to do.
type dockerHarness struct {
	probe        error
	unitActive   bool
	restartFixes bool // a restart makes the probe answer again
	restarted    int
	resetFailed  int
	restartErr   error
	now          time.Time
	out          bytes.Buffer
}

func (h *dockerHarness) deps() dockerWatchDeps {
	return dockerWatchDeps{
		Probe:      func(context.Context) error { return h.probe },
		UnitActive: func(context.Context) (bool, error) { return h.unitActive, nil },
		Restart: func(context.Context) error {
			h.restarted++
			if h.restartFixes {
				h.probe = nil
			}
			return h.restartErr
		},
		ResetFailed: func(context.Context) error { h.resetFailed++; return nil },
		Out:         &h.out,
		Now:         func() time.Time { return h.now },
	}
}

func newDockerHarness(t *testing.T) (*dockerHarness, string) {
	t.Helper()
	h := &dockerHarness{unitActive: true, now: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	return h, filepath.Join(t.TempDir(), dockerWatchStateFile)
}

// failPasses drives the check and returns the verdict of the last pass.
func failPasses(t *testing.T, h *dockerHarness, state string, n int) dockerVerdict {
	t.Helper()
	var last dockerVerdict
	for i := 0; i < n; i++ {
		last, _ = watchDockerOnce(context.Background(), state, h.deps())
	}
	return last
}

// TestDockerWatchRestartsAHungDaemon: the unit is `active` but the socket does not
// answer — a wedged dockerd. Three consecutive failures is the trigger.
func TestDockerWatchRestartsAHungDaemon(t *testing.T) {
	h, state := newDockerHarness(t)
	h.probe = errors.New("dial unix /var/run/docker.sock: connect: no such file or directory")

	if v := failPasses(t, h, state, DockerWatchFailuresBeforeRestart-1); v != DockerVerdictRecorded {
		t.Fatalf("verdict before the trigger = %s, want failure-recorded", v)
	}
	if h.restarted != 0 {
		t.Fatalf("restarted after %d failure(s), want %d", DockerWatchFailuresBeforeRestart-1, DockerWatchFailuresBeforeRestart)
	}

	// The restart's verification probe still fails, so this pass reports the node as
	// needing an operator. That is the honest outcome for a daemon that never came
	// back, and it must not be reported as success.
	if _, err := watchDockerOnce(context.Background(), state, h.deps()); err == nil {
		t.Fatal("a daemon that never answered after a restart was reported as recovered")
	}
	if h.restarted != 1 {
		t.Fatalf("restarted %d time(s), want exactly 1", h.restarted)
	}
	if h.resetFailed != 0 {
		t.Fatal("reset-failed was called for an ACTIVE unit; a restart is enough")
	}
	if !strings.Contains(h.out.String(), "STILL does not answer") {
		t.Fatalf("the journal does not say the node needs an operator:\n%s", h.out.String())
	}
}

// TestDockerWatchReArmsAUnitSystemdGaveUpOn is the case that took nxt-sw-3-w out of
// the cluster: the daemon was killed, systemd hit StartLimitBurst, the unit stayed
// `failed` — so a plain restart is refused and the node never recovers on its own.
func TestDockerWatchReArmsAUnitSystemdGaveUpOn(t *testing.T) {
	h, state := newDockerHarness(t)
	h.unitActive = false
	h.restartFixes = true
	h.probe = errors.New("connect: connection refused")

	if v := failPasses(t, h, state, DockerWatchFailuresBeforeRestart); v != DockerVerdictReArmed {
		t.Fatalf("verdict = %s, want reset-failed-and-restarted", v)
	}
	if h.resetFailed != 1 {
		t.Fatalf("reset-failed called %d time(s), want 1: without it systemd refuses the restart", h.resetFailed)
	}
	if h.restarted != 1 {
		t.Fatalf("restarted %d time(s), want 1", h.restarted)
	}
	if !strings.Contains(h.out.String(), "answers again after the restart") {
		t.Fatalf("a successful recovery was not reported:\n%s", h.out.String())
	}
}

// TestDockerWatchRateLimitStopsABouncingDaemon: three failures inside the floor must
// not produce a second restart. A dockerd that dies repeatedly is a bug to investigate,
// not a service to keep bouncing.
func TestDockerWatchRateLimitStopsABouncingDaemon(t *testing.T) {
	h, state := newDockerHarness(t)
	h.probe = errors.New("connect: connection refused")

	failPasses(t, h, state, DockerWatchFailuresBeforeRestart)
	if h.restarted != 1 {
		t.Fatalf("restarted %d time(s) before the floor elapsed, want 1", h.restarted)
	}

	// Ten minutes later: still inside docker's 30 minute floor (and outside the
	// pmcluster watchdog's 10 minute one).
	h.now = h.now.Add(10 * time.Minute)
	if v := failPasses(t, h, state, DockerWatchFailuresBeforeRestart); v != DockerVerdictRateLimited {
		t.Fatalf("verdict = %s, want rate-limited (docker's floor is 30m, not pmcluster's 10m)", v)
	}
	if h.restarted != 1 {
		t.Fatalf("restarted %d time(s), want 1 — the rate limit did not hold", h.restarted)
	}
}

// TestDockerWatchResetsOnRecovery: a healthy probe clears the counter, so a single blip
// can never accumulate into a restart over hours.
func TestDockerWatchResetsOnRecovery(t *testing.T) {
	h, state := newDockerHarness(t)
	h.probe = errors.New("blip")
	_, _ = watchDockerOnce(context.Background(), state, h.deps())

	h.probe = nil
	if v, err := watchDockerOnce(context.Background(), state, h.deps()); err != nil || v != DockerVerdictHealthy {
		t.Fatalf("verdict = %s (%v), want healthy", v, err)
	}
	st, _ := loadWatchdogState(state)
	if st.Failures != 0 {
		t.Fatalf("failures = %d after recovery, want 0", st.Failures)
	}
}

// TestWatchdogServiceOnlyWatchesDockerWhenEnabled: the flag is the whole difference
// between a watchdog that can restart dockerd and one that cannot, so both shapes are
// asserted.
func TestWatchdogServiceOnlyWatchesDockerWhenEnabled(t *testing.T) {
	off := renderWatchdogServiceWithDocker("/usr/local/bin/pmcluster", "/root/.pmcluster/watchdog.json",
		defaultWatchdogProbeURL, "")
	if strings.Contains(off, "--docker-socket") {
		t.Fatalf("the default unit already watches docker:\n%s", off)
	}
	on := renderWatchdogServiceWithDocker("/usr/local/bin/pmcluster", "/root/.pmcluster/watchdog.json",
		defaultWatchdogProbeURL, defaultDockerSocket)
	if !strings.Contains(on, "--docker-socket "+defaultDockerSocket) {
		t.Fatalf("the enabled unit does not pass the socket:\n%s", on)
	}
	// Capability and permission are separate flags: the socket is what the watchdog
	// CAN reach, the policy file is what the leader SAID. A unit with the socket and
	// no policy stays disarmed, which is the safe ship state.
	if !strings.Contains(on, "--policy-file /root/.pmcluster/"+nodePolicyFileName) {
		t.Fatalf("the unit does not read the node policy document:\n%s", on)
	}
}

// TestNodePolicyArmsDockerIsFailClosed: this decides whether a root timer restarts a
// node's docker daemon. Missing, unreadable, malformed and false must all mean "do
// nothing"; only an explicit true may arm it.
func TestNodePolicyArmsDockerIsFailClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, nodePolicyFileName)

	if nodePolicyArmsDocker("") {
		t.Fatal("an empty path armed docker")
	}
	if nodePolicyArmsDocker(path) {
		t.Fatal("a missing policy file armed docker")
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if nodePolicyArmsDocker(path) {
		t.Fatal("a malformed policy file armed docker")
	}
	if err := os.WriteFile(path, []byte(`{"docker_restart":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if nodePolicyArmsDocker(path) {
		t.Fatal("an explicit false armed docker")
	}
	if err := os.WriteFile(path, []byte(`{"docker_restart":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !nodePolicyArmsDocker(path) {
		t.Fatal("an explicit true did not arm docker")
	}
}

// TestHealControlPlaneAfterDockerRestart is the regression for the last link in the
// docker-watchdog chain: the watchdog restarts docker, `PartOf` takes this node's
// daemon down with it, and the outage has already spent the unit's start budget — so
// systemd answers "start request repeated too quickly" and leaves it `failed`, and
// nothing starts a failed unit when docker returns (nxt-sw-6-w, 2026-11-13).
func TestHealControlPlaneAfterDockerRestart(t *testing.T) {
	t.Run("a daemon that survived the restart is left alone", func(t *testing.T) {
		calls := 0
		healed, err := healControlPlaneAfterDockerRestart(context.Background(), healDeps{
			UnitActive:  func(context.Context) (bool, error) { return true, nil },
			ResetFailed: func(context.Context) error { calls++; return nil },
			Start:       func(context.Context) error { calls++; return nil },
		})
		if err != nil || healed {
			t.Fatalf("healed=%v err=%v, want false/nil", healed, err)
		}
		if calls != 0 {
			t.Fatalf("systemd calls = %d, want 0 (an active unit is not touched)", calls)
		}
	})

	t.Run("reset-failed runs before the start, and a reset failure is not fatal", func(t *testing.T) {
		var order []string
		var out bytes.Buffer
		healed, err := healControlPlaneAfterDockerRestart(context.Background(), healDeps{
			UnitActive:  func(context.Context) (bool, error) { return false, nil },
			ResetFailed: func(context.Context) error { order = append(order, "reset-failed"); return errors.New("nope") },
			Start:       func(context.Context) error { order = append(order, "start"); return nil },
			Out:         &out,
		})
		if err != nil {
			t.Fatalf("err = %v, want nil (a refused reset must not stop the start)", err)
		}
		if !healed {
			t.Fatal("healed = false, want true")
		}
		if strings.Join(order, ",") != "reset-failed,start" {
			t.Fatalf("order = %v, want reset-failed then start", order)
		}
		if !strings.Contains(out.String(), "was down after the docker restart; started it") {
			t.Fatalf("no journal line for the heal:\n%s", out.String())
		}
	})

	t.Run("a start that fails is reported", func(t *testing.T) {
		_, err := healControlPlaneAfterDockerRestart(context.Background(), healDeps{
			UnitActive: func(context.Context) (bool, error) { return false, nil },
			Start:      func(context.Context) error { return errors.New("start request repeated too quickly") },
		})
		if err == nil || !strings.Contains(err.Error(), "after the docker restart") {
			t.Fatalf("err = %v, want the start failure surfaced", err)
		}
	})
}
