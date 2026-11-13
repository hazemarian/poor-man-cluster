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
func convergeSystemd(logf func(string, ...any), dockerSocket, dataDir string) {
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
	memChanged, err := writeMemoryDropIn(mem)
	if err != nil {
		logf("systemd convergence: cannot write %s: %v", stopUnitDropIn, err)
	} else if memChanged {
		if mem.TotalMiB > 0 {
			logf("memory bounds written for this host (MemoryHigh=%dM, MemoryMax=%dM of %d MiB) — they apply on the next restart",
				mem.HighMiB, mem.MaxMiB, mem.TotalMiB)
		} else {
			logf("memory bounds: this host's MemTotal could not be read, so only OOMScoreAdjust was written")
		}
	}

	// The dependency override is separate from the memory bounds because it fixes a
	// different fault: a unit that is old enough to have BindsTo. Same rule as the
	// memory drop-in — the daemon owns it, and it is rewritten from scratch each start.
	reload := false
	if changed, err := writeDependencyDropIn(); err != nil {
		logf("systemd convergence: cannot write %s: %v", dependencyDropIn, err)
	} else if changed {
		logf("docker dependency written (PartOf=docker.service) — a docker restart now restarts this agent")
		reload = true
	}
	// …and the BindsTo itself, which no drop-in can take away.
	if changed, err := repairDaemonUnitDependency(); err != nil {
		logf("systemd convergence: cannot repair %s: %v (a dockerd outage will still stop this agent)", daemonUnitPath, err)
	} else if changed {
		logf("docker dependency repaired: removed BindsTo=docker.service from %s — this agent no longer goes down with dockerd", daemonUnitPath)
		reload = true
	}
	if memChanged {
		reload = true
	}
	if reload {
		if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
			logf("systemd convergence: daemon-reload failed: %v", err)
		}
	}

	// Independent of the unit, and effective at once. enable --now is
	// idempotent, which is what makes a node that predates the watchdog watch.
	var buf bytes.Buffer
	if dataDir == "" {
		// Same fallback the agent uses: one directory for the watchdog state, the
		// applied assignment and the node policy, so the two processes cannot end up
		// reading different files.
		dataDir = filepath.Join(os.Getenv("HOME"), ".pmcluster")
	}
	installWatchdog(&buf, exe, dataDir, dockerSocket)
	if out := strings.TrimSpace(buf.String()); out != "" {
		logf("watchdog: %s", strings.ReplaceAll(out, "\n", "; "))
	}
	if err := exec.Command("systemctl", "enable", "--now", "pmcluster-watchdog.timer").Run(); err != nil {
		logf("systemd convergence: could not enable the watchdog timer: %v", err)
	}
}

// dependencyDropIn overrides the unit's dependency on docker.
//
// The main unit is rendered once at install/join and an upgrade never re-renders
// it, so a node built before v0.2.290 keeps BindsTo=docker.service forever — the
// binary is new, the unit is old.
//
// A drop-in CANNOT fix that. This one used to carry an empty `BindsTo=` on the
// belief that an empty assignment resets an inherited dependency list; measured on
// systemd 259 (2026-11-13, nxt-sw-6-w) it does not — after `daemon-reload`,
// `systemctl show pmcluster -p BindsTo` still answered `BindsTo=docker.service`,
// and a minimal scratch unit reproduced it. So the inherited directive is removed by
// repairing the unit file itself (repairDaemonUnitDependency), and this drop-in only
// adds what a drop-in can add: PartOf, which propagates a restart of docker to this
// unit.
var dependencyDropIn = "/etc/systemd/system/pmcluster.service.d/docker-dependency.conf"

// writeDependencyDropIn writes the dependency override and reports whether the
// file changed.
func writeDependencyDropIn() (bool, error) {
	const want = `# Managed by pmcluster — do not edit: rewritten on every daemon start.
# PartOf propagates a RESTART of docker to this unit, which is what a docker
# restart needs: without it the node agent stays down while dockerd comes back up,
# every docker call inside a CI job blocks forever, and the single ephemeral runner
# stays pinned for hours with the console showing "queued".
# Nothing here cancels BindsTo: an empty assignment does NOT reset a dependency
# inherited from the main unit (measured on systemd 259), so that directive is
# removed from the unit file by repairDaemonUnitDependency instead.
[Unit]
PartOf=docker.service
`
	existing, err := os.ReadFile(dependencyDropIn)
	if err == nil && string(existing) == want {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dependencyDropIn), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(dependencyDropIn, []byte(want), 0o644)
}

// repairDaemonUnitDependency removes an inherited `BindsTo=docker.service` from the
// daemon's unit file, preserving every other line.
//
// This is the half the drop-in cannot do, and it is the difference between a node
// that heals and one that does not. With BindsTo in force, a dockerd that fails is
// propagated as a STOP of pmcluster, and systemd ignores Restart=always for a stop a
// dependency caused — so the node comes back with docker running and no control
// plane at all. Measured end-to-end (2026-11-13, nxt-sw-6-w): the docker watchdog
// did its job (3 failed probes → reset-failed → restart docker → docker answering),
// and pmcluster was still `inactive` afterwards, because it had been stopped at the
// first kill and nothing was left to start it.
//
// The unit is edited rather than re-rendered: re-rendering means guessing the user,
// group and ExecStart the node was installed with, and guessing wrong takes the
// service down. Removing one directive from the file that is already there cannot.
//
// A node whose unit predates the docker check has no BindsTo at all, so this is a
// no-op there (and reports no change, so nothing churns daemon-reload).
func repairDaemonUnitDependency() (bool, error) {
	body, err := os.ReadFile(daemonUnitPath)
	if err != nil {
		return false, err
	}
	lines := strings.Split(string(body), "\n")
	kept := make([]string, 0, len(lines))
	changed := false
	for _, line := range lines {
		indent := line[:len(line)-len(strings.TrimLeft(line, " 	"))]
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "BindsTo") {
			kept = append(kept, line)
			continue
		}
		var remaining []string
		for _, tok := range strings.Fields(value) {
			if tok == "docker.service" {
				changed = true
				continue
			}
			remaining = append(remaining, tok)
		}
		if len(remaining) == 0 {
			// The directive existed only to bind docker: drop the whole line.
			continue
		}
		kept = append(kept, indent+key+"="+strings.Join(remaining, " "))
	}
	if !changed {
		return false, nil
	}

	// Write through a temp file and rename: systemd may reload at any moment, and a
	// half-written unit is a service that does not start.
	dir := filepath.Dir(daemonUnitPath)
	tmp, err := os.CreateTemp(dir, ".pmcluster-unit-*.tmp")
	if err != nil {
		return false, err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	if _, err := tmp.WriteString(strings.Join(kept, "\n")); err != nil {
		return false, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), daemonUnitPath)
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
