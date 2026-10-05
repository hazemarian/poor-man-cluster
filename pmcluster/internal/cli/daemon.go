package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// renderSystemdUnit builds the pmcluster systemd unit file. The daemon is
// managed by the CLI (cluster up/update, join) — install.sh never writes it.
func renderSystemdUnit(pmUser, group, home, execStart string) string {
	return fmt.Sprintf(`[Unit]
Description=pmcluster API Server
After=docker.service
Requires=docker.service

[Service]
Type=simple
User=%s
Group=%s
Environment=HOME=%s
ExecStart=%s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, pmUser, group, home, execStart)
}

// daemonUnitPath is the systemd unit location. Overridable in tests.
var daemonUnitPath = "/etc/systemd/system/pmcluster.service"

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

	for _, args := range [][]string{{"daemon-reload"}, {"enable", "pmcluster"}} {
		if err := systemctlFn(args...); err != nil {
			return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
		}
	}
	if err := systemctlFn("restart", "pmcluster"); err != nil {
		return fmt.Errorf("systemctl restart pmcluster: %w", err)
	}
	fmt.Fprintln(out, "→ pmcluster daemon started via systemd (pmcluster serve)")
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
