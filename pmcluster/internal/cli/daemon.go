package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
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
	if runtime.GOOS != "linux" {
		fmt.Fprintln(out, "  (non-Linux host — start the daemon with: pmcluster serve)")
		return nil
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		fmt.Fprintln(out, "  (systemctl not found — start the daemon with: pmcluster serve)")
		return nil
	}

	// Resolve the user the daemon runs as: PMCLUSTER_USER, then SUDO_USER,
	// then the current user (mirrors install.sh's detection order).
	pmUser := os.Getenv("PMCLUSTER_USER")
	if pmUser == "" {
		pmUser = os.Getenv("SUDO_USER")
	}
	home := os.Getenv("HOME")
	if pmUser == "" {
		if u, err := user.Current(); err == nil {
			pmUser = u.Username
			if home == "" {
				home = u.HomeDir
			}
		}
	}
	if pmUser == "" {
		pmUser = "root"
	}
	if home == "" {
		home = "/root"
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
		if err := writeFileSudo(daemonUnitPath, []byte(unit), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", daemonUnitPath, err)
		}
		fmt.Fprintf(out, "→ installed systemd unit %s (user %s, group %s)\n", daemonUnitPath, pmUser, group)
	}

	for _, args := range [][]string{{"daemon-reload"}, {"enable", "pmcluster"}} {
		if err := systemctlSudo(args...); err != nil {
			return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
		}
	}
	if err := systemctlSudo("restart", "pmcluster"); err != nil {
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
	cmd.Stdout = io.Discard
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
