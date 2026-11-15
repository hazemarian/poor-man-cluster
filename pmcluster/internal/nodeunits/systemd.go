// Package nodeunits owns the systemd units a pmcluster node installs on itself,
// and the node-local files those units read.
//
// It exists because that work was split across the CLI: the daemon's own unit was
// rendered in internal/cli/daemon.go while the backup agent's six units were
// rendered in internal/cli/backup_unit.go, next to forty cobra commands — so a
// systemd bug (a PartOf/BindsTo interaction, a drop-in that could not cancel one)
// was hunted for in a package nobody opens expecting to find systemd, and the two
// installers could not share the seams their tests need.
//
// Nothing here talks to the control plane beyond reading this node's settings, and
// nothing here is a cobra command: the CLI calls in, the daemon calls in, and the
// unit text is testable with no cluster.
package nodeunits

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// HostOS is the platform the units are for. Overridable in tests, which is the
// only way to assert "a laptop must not install a timer" on a CI box.
var HostOS = runtime.GOOS

// HasSystemctl reports whether systemctl exists on PATH. Overridable in tests.
var HasSystemctl = func() bool {
	_, err := exec.LookPath("systemctl")
	return err == nil
}

// Systemctl runs a systemctl subcommand (sudo-prefixed when not root).
// Overridable in tests so no test touches a real init system.
var Systemctl = SystemctlSudo

// WriteUnit writes a unit file. Defaults to writeFileSudo (direct when root,
// sudo-tee otherwise); overridable in tests to avoid sudo.
var WriteUnit = WriteFileSudo

// WriteFileSudo writes a file, prefixing sudo when the current process is not
// root (systemd units live under /etc, owned by root).
func WriteFileSudo(path string, data []byte, mode os.FileMode) error {
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

// SystemctlSudo runs a systemctl subcommand, prefixing sudo when not root.
func SystemctlSudo(args ...string) error {
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

// CanInstall reports whether this host can carry a unit at all, and why not when
// it cannot. Callers print the reason rather than inventing their own.
func CanInstall() (bool, string) {
	if HostOS != "linux" {
		return false, "the unit is a Linux systemd unit"
	}
	if !HasSystemctl() {
		return false, "systemctl not found — run the command from your own scheduler"
	}
	return true, ""
}
