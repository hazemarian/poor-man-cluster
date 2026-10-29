package e2e

// Test harness — the ONE place that knows what the environment must provide.
//
// The suite has to behave identically for a developer running `make e2e` on a
// laptop and for CI, without the developer knowing CI's private switches. Two
// rules follow, and every test must obey them:
//
//  1. REQUIRE BY CAPABILITY, NOT BY ENV VAR. `requireSwarm(t)` probes the engine
//     and skips with a reason naming what is missing. There is no magic variable
//     to remember: if a Docker engine with Swarm is reachable, the test runs.
//
//  2. TIMEOUTS ARE ENVIRONMENT-SCALED. A laptop running dockerd inside a VM —
//     or pulling images cold — is several times slower than a CI runner with a
//     warm cache. `e2eTimeout()` applies that factor in one place instead of
//     every test hardcoding a number that is wrong somewhere.
//
// PMCLUSTER_E2E_SWARM is still honoured, but only as an OVERRIDE:
//   =0  force skip (e.g. a laptop that must not touch its daemon)
//   =1  force run and fail hard when the environment is missing, rather than
//       skip — this is what CI sets, so a broken runner cannot pass quietly.
//
// PMCLUSTER_E2E_SLOW multiplies every wait (default 1; 2-3 on emulated or
// cold-image machines).

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// defaultTimeoutScale is used when PMCLUSTER_E2E_SLOW is unset or invalid.
const defaultTimeoutScale = 1.0

// e2eTimeout scales a wait for the current environment.
//
// It exists so a slow machine gets a longer timeout instead of a false failure,
// and so CI does not have to carry a separate set of numbers. Callers keep
// passing the *CI-baseline* duration, not a tuned one.
func e2eTimeout(d time.Duration) time.Duration {
	scale := defaultTimeoutScale
	if v := strings.TrimSpace(os.Getenv("PMCLUSTER_E2E_SLOW")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			scale = f
		}
	}
	return time.Duration(float64(d) * scale)
}

// dockerAvailable reports whether a docker CLI is on PATH at all.
func dockerAvailable() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

// engineReachable reports whether the daemon answers. An unreachable daemon is
// a different failure from a missing one, and both read differently in a log.
func engineReachable() (bool, string) {
	cmd := exec.Command("docker", "info", "--format", "{{.ServerVersion}}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, strings.TrimSpace(string(out))
	}
	return true, strings.TrimSpace(string(out))
}

// swarmState returns the engine's swarm mode: "active", "inactive", "locked",
// or "error" with a reason.
func swarmState() (string, string) {
	cmd := exec.Command("docker", "info", "--format", "{{.Swarm.LocalNodeState}}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "error", strings.TrimSpace(string(out))
	}
	return strings.TrimSpace(string(out)), ""
}

// swarmTierEnabled reports whether swarm-gated work should run, using the SAME
// detection as requireSwarm: an explicit override wins, otherwise capability.
//
// TestMain needs this to decide whether to build the local edge image — with
// capability-based detection, a developer whose engine is available runs the
// swarm tier WITHOUT setting any variable, so keying the image build off the
// variable alone would silently test a stale published image.
func swarmTierEnabled() bool {
	switch os.Getenv("PMCLUSTER_E2E_SWARM") {
	case "0":
		return false
	case "1":
		return true
	}
	if !dockerAvailable() {
		return false
	}
	if ok, _ := engineReachable(); !ok {
		return false
	}
	// Active only: an inactive engine will SKIP the swarm tier (see requireSwarm),
	// so building the local edge image for it would be wasted work in the smoke
	// job. Forced mode is handled by the switch above.
	state, _ := swarmState()
	return state == "active"
}

// ensureSwarm makes a single-node swarm exist, which is what every swarm test
// needs. CI and a laptop follow the same path here — the tests used to do this
// inline with slightly different wording each time.
func ensureSwarm(t *testing.T) error {
	t.Helper()
	state, reason := swarmState()
	switch state {
	case "active":
		return nil
	case "inactive":
		cmd := exec.Command("docker", "swarm", "init", "--advertise-addr", "127.0.0.1")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("docker swarm init: %v: %s", err, strings.TrimSpace(string(out)))
		}
		t.Log("swarm was inactive; initialised a single-node swarm for this suite")
		return nil
	default:
		return fmt.Errorf("swarm state %q: %s", state, reason)
	}
}

// requireSwarm skips the test unless a Swarm-capable engine is usable, and
// explains exactly what is missing when it is not.
//
// The forced-run mode (PMCLUSTER_E2E_SWARM=1) FAILS instead of skipping: that is
// the mode CI uses, so an environment that lost its Docker cannot turn the whole
// suite green by skipping everything.
func requireSwarm(t *testing.T) {
	t.Helper()

	if os.Getenv("PMCLUSTER_E2E_SWARM") == "0" {
		t.Skip("PMCLUSTER_E2E_SWARM=0: swarm e2e explicitly disabled on this machine")
	}
	forced := os.Getenv("PMCLUSTER_E2E_SWARM") == "1"

	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if forced {
			t.Fatalf("%s\n\nPMCLUSTER_E2E_SWARM=1 means 'run for real', so this is a "+
				"failure rather than a skip. Unset it to let the suite skip instead.", msg)
		}
		t.Skip(msg)
	}

	if !dockerAvailable() {
		fail("no docker CLI on PATH: install one and point DOCKER_HOST at a Swarm-capable engine " +
			"(docker desktop, colima, a throwaway VM, or a privileged docker:dind container)")
	}
	ok, out := engineReachable()
	if !ok {
		fail("docker CLI found but the engine is unreachable: %s\n"+
			"hint: DOCKER_HOST=%s", out, os.Getenv("DOCKER_HOST"))
	}
	// DETECT, do not provision. Initialising a swarm is a side effect, and a
	// smoke-tier test that asserts "no cluster data yet" changes behaviour the
	// moment one exists — it starts exiting 0 instead of demanding a wizard, and
	// the failure looks like a product bug rather than test scaffolding.
	//
	// So an inactive engine only gets initialised when the caller has explicitly
	// asked for the swarm tier (PMCLUSTER_E2E_SWARM=1, CI's mode). Everyone else
	// is told how to prepare the engine themselves.
	state, reason := swarmState()
	if state != "active" {
		if forced {
			if err := ensureSwarm(t); err != nil {
				fail("cannot prepare a swarm: %v", err)
			}
		} else {
			fail("swarm is %q (%s): this suite must not mutate the engine's swarm "+
				"state. Run `docker swarm init` yourself, or set "+
				"PMCLUSTER_E2E_SWARM=1 to let the suite initialise one.", state, reason)
		}
	}
	t.Logf("harness: docker %s, swarm active, timeout scale %.1fx",
		out, float64(e2eTimeout(time.Second))/float64(time.Second))
}

// TestE2EEnvironment always runs and reports what the suite detected. It is the
// answer to "why did everything skip?" without reading eleven skip messages.
func TestE2EEnvironment(t *testing.T) {
	hasDocker := dockerAvailable()
	t.Logf("docker CLI on PATH: %v", hasDocker)
	if hasDocker {
		if ok, out := engineReachable(); ok {
			state, _ := swarmState()
			t.Logf("engine reachable: server %s, swarm %s", out, state)
		} else {
			t.Logf("engine NOT reachable: %s", out)
		}
	}
	t.Logf("timeout scale: %.1fx (PMCLUSTER_E2E_SLOW=%q, default %0.1f)",
		float64(e2eTimeout(time.Second))/float64(time.Second),
		os.Getenv("PMCLUSTER_E2E_SLOW"), defaultTimeoutScale)
	t.Logf("swarm gate: PMCLUSTER_E2E_SWARM=%q (\"\"=auto, 1=force run, 0=force skip)",
		os.Getenv("PMCLUSTER_E2E_SWARM"))
}
