package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// stopUnitDropIn is where the memory bounds live. A drop-in is merged by systemd
// and cannot clobber ExecStart/User, so converging it from inside the daemon is
// safe on any node regardless of how it was installed.
// Overridable in tests, like daemonUnitPath.
var stopUnitDropIn = "/etc/systemd/system/pmcluster.service.d/memory.conf"

// convergeSystemd keeps this node's systemd protection in step with the binary
// that is actually running.
//
// install and join render the main unit ONCE, when the node is built. An upgrade
// replaces the binary and restarts the service but never re-renders, so a node
// that has not run `cluster up` since keeps whatever it was born with. Measured
// after the watchdog shipped: the leader had MemoryHigh/MemoryMax and an active
// pmcluster-watchdog.timer, while the worker that had just been OOM-killed still
// had MemoryMax=infinity, OOMScoreAdjust=0 and no timer — the one node that
// needed the protection was the one without it.
//
// It converges the two things that are safe to own from inside the daemon:
//
//  1. a DROP-IN with the memory bounds. A drop-in is merged by systemd and
//     cannot clobber ExecStart/User, which is why the main unit is not rewritten
//     here — re-rendering it would mean guessing the user and group the node was
//     installed with, and guessing wrong takes the service down.
//  2. the watchdog timer, which takes effect immediately and needs no restart.
//
// The bounds themselves only apply on the next restart (the daemon cannot move
// its own running process into a new memory cgroup without systemd's consent),
// so when they change it says so instead of pretending.
//
// Best effort by design: a container without systemd, a dev box or a non-root run
// must start normally, so every failure is a log line, never an error.
func convergeSystemd(logf func(string, ...any)) {
	if os.Geteuid() != 0 {
		return
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return
	}
	// Only a systemd-managed install has a unit to attach to.
	if _, err := os.Stat(daemonUnitPath); err != nil {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		logf("systemd convergence skipped: cannot resolve the binary path: %v", err)
		return
	}

	mem := memoryLimitsFor(hostMemTotalMiB())
	if changed, err := writeMemoryDropIn(mem); err != nil {
		logf("systemd convergence: cannot write %s: %v", stopUnitDropIn, err)
	} else if changed {
		if mem.TotalMiB > 0 {
			logf("memory bounds written for this host (MemoryHigh=%dM, MemoryMax=%dM of %d MiB) — they apply on the next restart",
				mem.HighMiB, mem.MaxMiB, mem.TotalMiB)
		} else {
			logf("memory bounds: this host's MemTotal could not be read, so only OOMScoreAdjust was written")
		}
		if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
			logf("systemd convergence: daemon-reload failed: %v", err)
		}
	}

	// Independent of the unit, and effective at once. enable --now is
	// idempotent, which is what makes a node that predates the watchdog watch.
	var buf bytes.Buffer
	installWatchdog(&buf, exe, filepath.Join(os.Getenv("HOME"), ".pmcluster"))
	if out := strings.TrimSpace(buf.String()); out != "" {
		logf("watchdog: %s", strings.ReplaceAll(out, "\n", "; "))
	}
	if err := exec.Command("systemctl", "enable", "--now", "pmcluster-watchdog.timer").Run(); err != nil {
		logf("systemd convergence: could not enable the watchdog timer: %v", err)
	}
}

// writeMemoryDropIn writes the memory bounds and reports whether the file changed.
//
// The drop-in is the ONLY place these numbers live. They used to be rendered into
// the main unit as well, which is how a node ends up with two answers to "what is
// the limit?" — the unit's and the drop-in's, with the drop-in winning only
// because systemd loads it later. One owner, and it is the one that is recomputed
// from /proc/meminfo on every start, so a resized host is picked up without anyone
// remembering to re-render the unit.
//
// OOMScoreAdjust is written even when MemTotal is unreadable: -500 does not depend
// on knowing the host's size, and the ordering is worth having either way.
func writeMemoryDropIn(mem MemoryLimits) (bool, error) {
	var want string
	if mem.TotalMiB > 0 {
		want = fmt.Sprintf(`# Managed by pmcluster — do not edit: rewritten on every daemon start.
# 60%% / 70%% of this host's %d MiB. A limit larger than the host is not a limit:
# on a 1839 MB node a 2560m runner cap was permission to OOM the host and take it
# Down with sshd.
[Service]
MemoryHigh=%dM
MemoryMax=%dM
# Prefer the WORKLOAD over the control plane when the kernel picks a victim.
OOMScoreAdjust=-500
`, mem.TotalMiB, mem.HighMiB, mem.MaxMiB)
	} else {
		want = `# Managed by pmcluster — do not edit: rewritten on every daemon start.
# This host's MemTotal could not be read, so no memory limit is written; a guessed
# one could exceed the host. The OOM ordering does not depend on the host's size.
[Service]
OOMScoreAdjust=-500
`
	}
	existing, err := os.ReadFile(stopUnitDropIn)
	if err == nil && string(existing) == want {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(stopUnitDropIn), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(stopUnitDropIn, []byte(want), 0o644)
}
