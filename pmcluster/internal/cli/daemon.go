package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// The control plane's own memory boundaries.
//
// A limit that exceeds the host is not a limit: it is permission for the kernel
// to OOM-kill something else and call it enforcement. On 2026-11-10 a runner was
// allowed a 2560m cap on a 1839 MB node — the same arithmetic error — and the
// node died. The daemon's numbers are therefore COMPUTED from this host's
// MemTotal at install time, never written as constants.
//
// 60 % / 70 %: MemoryHigh is a throttle (the cgroup is reclaimed and slowed, the
// daemon is not killed), MemoryMax is the hard ceiling. The 10-point gap is the
// room the daemon needs to notice MemoryHigh and act — finish a write, return a
// 503 — instead of being killed the instant it crosses one line.
const (
	memoryHighPercent = 60
	memoryMaxPercent  = 70
)

// MemoryLimits are the cgroup bounds written into the daemon unit, with the
// total they were derived from (kept for the arithmetic in the log line and in
// tests). A zero TotalMiB means the host's RAM could not be read: nothing is
// written, because a guessed limit is exactly the failure this prevents.
type MemoryLimits struct {
	TotalMiB int64
	HighMiB  int64
	MaxMiB   int64
}

// memoryLimitsFor computes the boundaries from a host's total RAM in MiB.
//
// Rounded DOWN, and clamped so that High < Max < Total even on a tiny host: a
// Max equal to the host would let the cgroup consume everything the kernel needs
// to keep the SSH session alive, which is the state the two dead nodes were in.
func memoryLimitsFor(totalMiB int64) MemoryLimits {
	if totalMiB <= 0 {
		return MemoryLimits{}
	}
	l := MemoryLimits{TotalMiB: totalMiB}
	l.HighMiB = totalMiB * memoryHighPercent / 100
	l.MaxMiB = totalMiB * memoryMaxPercent / 100
	if l.MaxMiB >= totalMiB {
		l.MaxMiB = totalMiB - 1
	}
	if l.HighMiB >= l.MaxMiB {
		l.HighMiB = l.MaxMiB - (l.MaxMiB / 7) // keep High below Max on a small host
	}
	if l.HighMiB < 1 {
		l.HighMiB = 1
	}
	if l.MaxMiB < 2 {
		l.MaxMiB = 2
	}
	return l
}

// hostMemTotalMiB reads this host's total RAM, in MiB. Overridable in tests.
//
// /proc/meminfo, not `docker info`: the daemon's own cgroup boundary is a
// property of the machine it runs on, and Docker's number is what the ENGINE
// was told, which on a VM can be different.
var hostMemTotalMiB = func() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "MemTotal:" {
			continue
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || kb <= 0 {
			return 0
		}
		return kb / 1024
	}
	return 0
}

// renderSystemdUnit builds the pmcluster systemd unit file. The daemon is
// managed by the CLI (cluster up/update, join) — install.sh never writes it.
//
// PartOf=docker.service (not BindsTo=, not Requires=) is what propagates both a
// *stop* and a *restart* of docker to pmcluster. BindsTo was tried with
// WantedBy=docker.service and it is not enough: measured on a live node
// (2026-11-12, nxt-sw-6-w) `systemctl restart docker` stopped pmcluster and
// systemd left it `inactive (dead)` — Restart=always is not honored for a stop
// that a dependency caused, and the WantedBy pull-in does not fire on the start
// half of a restart.
//
// That failure is silent and expensive: pmcluster owns /run/pmcluster/docker.sock,
// so with it down every `docker` call in a runner job blocks forever, jobs hang
// instead of failing, and the single ephemeral runner is pinned for hours while
// the console shows "queued" and nothing runs. A node-level dependency must not
// be able to take the node's control loop away without a trace.
//
// The restart policy is deliberately bounded. `Restart=always` alone will spin
// as fast as the process exits: one node recorded 65 starts in a single boot
// (2026-11-08, nxt-sw-4-m), each start re-running startup work — Docker
// discovery, the control-plane check, the first telemetry export — on a 1-vCPU
// box that was already the one the swarm had just marked Down. RestartSec=10
// spaces attempts out; StartLimitBurst=5 inside StartLimitIntervalSec=300 caps
// them, after which systemd leaves the unit `failed` and says so, which is a
// state an operator can see and act on. A crash loop is a bug to fix, not a
// rhythm to keep; the unit must not be the loudest writer in the journal while
// somebody is trying to read it.
// renderSystemdUnit writes the unit WITHOUT memory bounds: they live in
// pmcluster.service.d/memory.conf, owned by the daemon's own convergence, because
// that file is recomputed from /proc/meminfo on every start. Two owners meant two
// answers to "what is the limit?" (see writeMemoryDropIn).
func renderSystemdUnit(pmUser, group, home, execStart string) string {
	return fmt.Sprintf(`[Unit]
Description=pmcluster API Server
After=docker.service
PartOf=docker.service
# Start-rate limit: at most 5 starts per 5 minutes, then stop trying and leave
# the unit failed (systemctl status shows the loop; 'systemctl reset-failed
# pmcluster' re-arms it once the cause is fixed).
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
User=%s
Group=%s
Environment=HOME=%s
ExecStart=%s
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
WantedBy=docker.service
`, pmUser, group, home, execStart)
}

// daemonUnitPath is the systemd unit location. Overridable in tests.
var daemonUnitPath = "/etc/systemd/system/pmcluster.service"

// watchdogUnitPaths returns the watchdog's two unit files. They live NEXT TO the
// daemon unit, so a test (or a host with a non-standard unit directory) keeps
// both in one place and nothing writes to /etc that was not asked to.
func watchdogUnitPaths() (service, timer string) {
	dir := filepath.Dir(daemonUnitPath)
	return filepath.Join(dir, "pmcluster-watchdog.service"),
		filepath.Join(dir, "pmcluster-watchdog.timer")
}

// renderWatchdogService builds pmcluster-watchdog.service: one oneshot run of
// the hidden health-watch subcommand, driven by the timer below.
//
// Root, because the one thing it does is `systemctl restart pmcluster`. The
// probe URL, the unit name and the state file are all passed explicitly: the
// watchdog must not need to read the operator's config (it runs as root, and a
// root process guessing at another user's data dir is how the wrong state file
// gets written).
//
// No After=/BindsTo=docker.service, unlike the daemon unit: a wedge is often
// caused by exactly the thing a dependency ordering would wait for.
func renderWatchdogService(execStart, statePath, probeURL string) string {
	return fmt.Sprintf(`[Unit]
Description=pmcluster health watchdog (restarts a wedged daemon)
# A watchdog that is rate-limited by systemd is a watchdog that stops watching:
# ten starts per five minutes is above the one-per-ten-minutes floor the
# command enforces, so this limit only ever catches a stuck unit.
StartLimitIntervalSec=300
StartLimitBurst=10

[Service]
Type=oneshot
User=root
ExecStart=%s health-watch --state-file %s --probe-url %s --unit pmcluster
# A probe is 5s and a restart is bounded by systemd; the ceiling exists so a
# hung systemctl cannot stack timer runs on top of each other.
TimeoutStartSec=120
# The control plane's own monitor yields to the workload it is watching.
Nice=5
`, execStart, statePath, probeURL)
}

// renderWatchdogTimer builds pmcluster-watchdog.timer: every 60 seconds, with
// the first run a minute and a half after boot (long enough for a slow daemon to
// bind, short enough that a node which boots wedged is fixed without a login).
func renderWatchdogTimer() string {
	return `[Unit]
Description=pmcluster health watchdog (every minute)

[Timer]
OnBootSec=90s
OnUnitActiveSec=60s
AccuracySec=5s
Unit=pmcluster-watchdog.service

[Install]
WantedBy=timers.target
`
}

// installWatchdog writes and enables the watchdog units for this node's data
// dir. A failure is a warning, never fatal: the daemon is already running by the
// time this is called, and refusing to finish an update because a timer could
// not be enabled would trade a degraded node for a broken one.
func installWatchdog(out io.Writer, exe, dataDir string) {
	servicePath, timerPath := watchdogUnitPaths()
	statePath := filepath.Join(dataDir, watchdogStateFile)
	units := []struct {
		path string
		body string
	}{
		{servicePath, renderWatchdogService(exe, statePath, defaultWatchdogProbeURL)},
		{timerPath, renderWatchdogTimer()},
	}
	changed := false
	for _, u := range units {
		existing, _ := os.ReadFile(u.path)
		if string(existing) == u.body {
			continue
		}
		if err := writeUnitFn(u.path, []byte(u.body), 0o644); err != nil {
			fmt.Fprintf(out, "  ⚠ health watchdog not installed: %v\n", err)
			return
		}
		changed = true
	}
	if changed {
		fmt.Fprintf(out, "→ installed health watchdog units %s, %s\n", servicePath, timerPath)
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "pmcluster-watchdog.timer"}, {"restart", "pmcluster-watchdog.timer"}} {
		if err := systemctlFn(args...); err != nil {
			fmt.Fprintf(out, "  ⚠ health watchdog may not run: systemctl %s: %v\n", strings.Join(args, " "), err)
			return
		}
	}
	fmt.Fprintf(out, "→ health watchdog active (probes %s every 60 s; restarts the unit after %d consecutive failures)\n",
		defaultWatchdogProbeURL, WatchdogFailuresBeforeRestart)
}

// rootDataDir is where the daemon's state lives when it was initialised under
// root (see ensureDaemonRunning's SUDO_USER fallback). Overridable in tests.
var rootDataDir = "/root/.pmcluster"

// systemctlFn runs a systemctl subcommand (sudo-prefixed when not root).
// Overridable in tests to avoid touching the real init system.
var systemctlFn = systemctlSudo

// hostOS is the platform the daemon would run on. Overridable in tests to
// exercise the Linux systemd flow on any host.
var hostOS = runtime.GOOS

// hasSystemctl reports whether systemctl exists on PATH. Overridable in tests.
var hasSystemctl = func() bool {
	_, err := exec.LookPath("systemctl")
	return err == nil
}

// writeUnitFn writes the systemd unit file. Defaults to writeFileSudo (direct
// when root, sudo-tee otherwise); overridable in tests to avoid sudo.
var writeUnitFn = writeFileSudo

// daemonExecStart returns the ExecStart command the systemd unit runs. The
// daemon is the `serve` subcommand — the unit must never run the bare binary
// (which would print help and exit, crash-looping the service).
func daemonExecStart(exe string) string {
	return exe + " serve"
}

// ensureDaemonRunning installs the pmcluster systemd unit (when missing or
// stale) and starts/restarts the daemon. On non-Linux hosts or systems without
// systemctl it prints a hint and returns nil — the daemon can always be run in
// the foreground with `pmcluster serve`.
//
// Executed from `pmcluster cluster up`, `pmcluster cluster update` and
// `pmcluster join`, so install.sh no longer needs to manage the service.
func ensureDaemonRunning(out io.Writer) error {
	if os.Getenv("PMCLUSTER_SKIP_DAEMON") != "" {
		// Test/e2e escape hatch: the harness starts its own foreground
		// daemon, and a stray systemd daemon on the same swarm would
		// restore + re-snapshot control-plane state with a fresh timestamp
		// (clobbering a just-minted admin token in shared-swarm tests).
		fmt.Fprintln(out, "  (PMCLUSTER_SKIP_DAEMON set — start the daemon with: pmcluster serve)")
		return nil
	}
	if hostOS != "linux" {
		fmt.Fprintln(out, "  (non-Linux host — start the daemon with: pmcluster serve)")
		return nil
	}
	if !hasSystemctl() {
		fmt.Fprintln(out, "  (systemctl not found — start the daemon with: pmcluster serve)")
		return nil
	}

	// Resolve the user the daemon runs as: PMCLUSTER_USER, then SUDO_USER,
	// then the current user (mirrors install.sh's detection order). A sudo
	// invocation (e.g. "sudo pmcluster cluster update") would otherwise pick
	// SUDO_USER even when the pmcluster state was initialised under root —
	// the daemon then crash-loops on "read /root/.pmcluster/config.yaml:
	// permission denied". The home is taken from the user's passwd entry
	// (NOT the HOME env var, which sudo resets to /root): if the resolved
	// user has no data dir of their own but root does, run as root.
	pmUser := os.Getenv("PMCLUSTER_USER")
	if pmUser == "" {
		pmUser = os.Getenv("SUDO_USER")
	}
	home := ""
	if pmUser == "" {
		if u, err := user.Current(); err == nil {
			pmUser = u.Username
			home = u.HomeDir
		}
	}
	if pmUser == "" {
		pmUser = "root"
	}
	if home == "" {
		if u, err := user.Lookup(pmUser); err == nil {
			home = u.HomeDir
		}
	}
	if home == "" {
		home = "/root"
	}
	if pmUser != "root" {
		// The data dir always lives at <home>/.pmcluster (config.DBPath).
		// If the resolved user has no accessible copy (missing OR
		// permission-denied — /root is 0700, so non-root processes get
		// EACCES, not ENOENT) but root does, the daemon must run as root.
		if _, err := os.Stat(filepath.Join(home, ".pmcluster")); err != nil {
			if _, rerr := os.Stat(rootDataDir); rerr == nil {
				pmUser, home = "root", "/root"
			}
		}
	}

	// Docker group: most distros use "docker"; some use "docker-root".
	group := "docker"
	if _, err := exec.LookPath("getent"); err == nil {
		if outb, err := exec.Command("getent", "group", "docker-root").CombinedOutput(); err == nil && strings.Contains(string(outb), "docker-root") {
			group = "docker-root"
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve binary path for unit: %w", err)
	}

	unit := renderSystemdUnit(pmUser, group, home, daemonExecStart(exe))

	// Write the unit only when missing or changed (operator edits win).
	existing, _ := os.ReadFile(daemonUnitPath)
	if string(existing) != unit {
		if err := writeUnitFn(daemonUnitPath, []byte(unit), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", daemonUnitPath, err)
		}
		fmt.Fprintf(out, "→ installed systemd unit %s (user %s, group %s)\n", daemonUnitPath, pmUser, group)
	}

	// The memory bounds go in a drop-in, which is also what the daemon rewrites on
	// every start. Writing them here as well is what made a fresh install and an
	// upgrade disagree about the same number.
	mem := memoryLimitsFor(hostMemTotalMiB())
	if changed, err := writeMemoryDropIn(mem); err != nil {
		fmt.Fprintf(out, "  ⚠ could not write %s: %v\n", stopUnitDropIn, err)
	} else if mem.TotalMiB > 0 {
		note := ""
		if changed {
			note = " (updated)"
		}
		fmt.Fprintf(out, "  memory bounds: MemoryHigh=%dM, MemoryMax=%dM (%d%%/%d%% of this host's %d MiB)%s\n",
			mem.HighMiB, mem.MaxMiB, memoryHighPercent, memoryMaxPercent, mem.TotalMiB, note)
	} else {
		fmt.Fprintf(out, "  ⚠ could not read this host's MemTotal: no MemoryHigh/MemoryMax written (a guessed limit could exceed the host)\n")
	}

	for _, args := range [][]string{{"daemon-reload"}, {"enable", "pmcluster"}} {
		if err := systemctlFn(args...); err != nil {
			return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
		}
	}
	if err := systemctlFn("restart", "pmcluster"); err != nil {
		return fmt.Errorf("systemctl restart pmcluster: %w", err)
	}
	fmt.Fprintln(out, "→ pmcluster daemon started via systemd (pmcluster serve)")

	// The watchdog goes in directly after the daemon, on every node (worker as
	// well as manager: a wedged worker daemon is just as unreachable). It is the
	// only actor that can restart a daemon that is alive and no longer
	// answering — see internal/cli/watchdog.go for the bounds on that.
	installWatchdog(out, exe, filepath.Join(home, ".pmcluster"))

	// The snapshot agent is installed only when the operator opts this node in.
	//
	// Opt-in, not automatic, because the two producers write different
	// archives and a node must not run both against the same data by
	// accident: the agent snapshots, the offen agent tars a live tree. Set
	// PMCLUSTER_BACKUP_AGENT=1 on a storage node that has the thin pool, watch
	// an hour of per-stack artifacts, then retire that node's offen agent.
	// Removing the offen agent before the agent is live would leave the node
	// with no working backup at all.
	if os.Getenv("PMCLUSTER_BACKUP_AGENT") != "" {
		return ensureBackupAgentInstalled(out)
	}
	return nil
}

// writeFileSudo writes a file, prefixing sudo when the current process is not
// root (systemd unit lives under /etc).
func writeFileSudo(path string, data []byte, mode os.FileMode) error {
	if os.Geteuid() == 0 {
		return os.WriteFile(path, data, mode)
	}
	cmd := exec.Command("sudo", "tee", path)
	cmd.Stdin = strings.NewReader(string(data))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sudo tee %s: %w (%s)", path, err, strings.TrimSpace(string(out)))
	}
	_ = exec.Command("sudo", "chmod", fmt.Sprintf("%o", mode), path).Run()
	return nil
}

// systemctlSudo runs a systemctl subcommand, prefixing sudo when not root.
func systemctlSudo(args ...string) error {
	argv := append([]string{"systemctl"}, args...)
	if os.Geteuid() != 0 {
		argv = append([]string{"sudo"}, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w (%s)", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// stopDaemon stops the pmcluster daemon via systemd, best-effort. Called before
// `cluster down --purge` so the daemon cannot resurrect swarm resources while
// the purge removes them. Only acts on Linux hosts with systemctl; a
// foreground daemon (or a non-systemd host) gets a hint and the purge proceeds.
// Never fatal.
func stopDaemon(out io.Writer) {
	if hostOS != "linux" || !hasSystemctl() {
		return
	}
	if err := systemctlFn("stop", "pmcluster"); err != nil {
		fmt.Fprintf(out, "  ⚠ could not stop the daemon: %v (continue — re-run purge after stopping it manually if resources reappear)\n", err)
		return
	}
	fmt.Fprintln(out, "→ pmcluster daemon stopped via systemd")
}
