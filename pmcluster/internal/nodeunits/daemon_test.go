package nodeunits

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSystemctl writes the received arguments into a log file so tests can
// assert the daemon management sequence without touching the real init system.
func fakeSystemctl(t *testing.T, logPath string) func(args ...string) error {
	t.Helper()
	return func(args ...string) error {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString(strings.Join(args, " ") + "\n")
		return err
	}
}

func TestWriteFileSudoAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise the direct-write path")
	}
	p := filepath.Join(t.TempDir(), "unit")
	if err := WriteFileSudo(p, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFileSudo: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil || string(got) != "hello" {
		t.Fatalf("read back = %q, %v; want hello", got, err)
	}
}

func TestEnsureDaemonRunningWritesUnitAndStarts(t *testing.T) {
	// Save + restore the overridable globals.
	oldPath := DaemonUnitPath
	oldSys := Systemctl
	oldHostOS := HostOS
	oldHasSys := HasSystemctl
	oldWrite := WriteUnit
	oldPMCLUSTER_USER, hadPM := os.LookupEnv("PMCLUSTER_USER")
	oldSUDO_USER, hadSUDO := os.LookupEnv("SUDO_USER")
	defer func() {
		DaemonUnitPath = oldPath
		Systemctl = oldSys
		HostOS = oldHostOS
		HasSystemctl = oldHasSys
		WriteUnit = oldWrite
		if hadPM {
			os.Setenv("PMCLUSTER_USER", oldPMCLUSTER_USER)
		} else {
			os.Unsetenv("PMCLUSTER_USER")
		}
		if hadSUDO {
			os.Setenv("SUDO_USER", oldSUDO_USER)
		} else {
			os.Unsetenv("SUDO_USER")
		}
	}()
	HostOS = "linux"
	HasSystemctl = func() bool { return true }
	WriteUnit = os.WriteFile
	os.Setenv("PMCLUSTER_USER", "testuser")

	unitPath := filepath.Join(t.TempDir(), "pmcluster.service")
	DaemonUnitPath = unitPath
	callLog := filepath.Join(t.TempDir(), "calls.log")
	Systemctl = fakeSystemctl(t, callLog)

	var out bytes.Buffer
	if err := EnsureDaemonRunning(&out, filepath.Join(t.TempDir(), "config.yaml")); err != nil {
		t.Fatalf("EnsureDaemonRunning: %v", err)
	}

	unit, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("unit not written: %v", err)
	}
	s := string(unit)
	for _, want := range []string{"Description=pmcluster API Server", "User=testuser", "Group=docker", "ExecStart=", " serve\n", "Restart=always"} {
		if !strings.Contains(s, want) {
			t.Errorf("unit missing %q:\n%s", want, s)
		}
	}
	calls, _ := os.ReadFile(callLog)
	for _, want := range []string{"daemon-reload", "enable pmcluster", "restart pmcluster"} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("systemctl calls missing %q:\n%s", want, calls)
		}
	}
	if !strings.Contains(out.String(), "pmcluster daemon started via systemd") {
		t.Errorf("output missing daemon-started message:\n%s", out.String())
	}
}

func TestEnsureDaemonRunningIdempotentUnit(t *testing.T) {
	oldPath := DaemonUnitPath
	oldSys := Systemctl
	oldHostOS := HostOS
	oldHasSys := HasSystemctl
	oldWrite := WriteUnit
	oldPM, hadPM := os.LookupEnv("PMCLUSTER_USER")
	defer func() {
		DaemonUnitPath = oldPath
		Systemctl = oldSys
		HostOS = oldHostOS
		HasSystemctl = oldHasSys
		WriteUnit = oldWrite
		if hadPM {
			os.Setenv("PMCLUSTER_USER", oldPM)
		} else {
			os.Unsetenv("PMCLUSTER_USER")
		}
	}()
	HostOS = "linux"
	HasSystemctl = func() bool { return true }
	WriteUnit = os.WriteFile
	os.Setenv("PMCLUSTER_USER", "testuser")

	unitPath := filepath.Join(t.TempDir(), "pmcluster.service")
	DaemonUnitPath = unitPath
	Systemctl = func(...string) error { return nil }

	var out1, out2 bytes.Buffer
	if err := EnsureDaemonRunning(&out1, filepath.Join(t.TempDir(), "config.yaml")); err != nil {
		t.Fatal(err)
	}
	// Second run: unit already matches → must NOT report "installed".
	if err := EnsureDaemonRunning(&out2, filepath.Join(t.TempDir(), "config.yaml")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out2.String(), "installed systemd unit") {
		t.Errorf("second run should not re-install the unit:\n%s", out2.String())
	}
}

func TestSystemctlSudoErrorWrapping(t *testing.T) {
	err := SystemctlSudo("bogus-command-xyz")
	if err == nil {
		t.Skip("systemctl accepted bogus command (unusual) — nothing to assert")
	}
	if !strings.Contains(err.Error(), "bogus-command-xyz") {
		t.Errorf("error should mention the command, got: %v", err)
	}
}

// TestEnsureDaemonRunning_FallsBackToRootDataDir verifies the sudo trap: a
// "sudo pmcluster cluster update" resolves SUDO_USER (e.g. wafaa) but the
// pmcluster state was initialised under root — the unit must run as root so
// the daemon can read /root/.pmcluster instead of crash-looping.
func TestEnsureDaemonRunning_FallsBackToRootDataDir(t *testing.T) {
	if os.Getenv("HOME") == "" {
		t.Skip("no HOME to compare against")
	}
	oldPath := DaemonUnitPath
	oldSys := Systemctl
	oldHasSys := HasSystemctl
	oldWrite := WriteUnit
	oldRoot := RootDataDir
	oldHost := HostOS
	oldSUDO := os.Getenv("SUDO_USER")
	t.Cleanup(func() {
		DaemonUnitPath = oldPath
		Systemctl = oldSys
		HasSystemctl = oldHasSys
		WriteUnit = oldWrite
		RootDataDir = oldRoot
		HostOS = oldHost
		os.Setenv("SUDO_USER", oldSUDO)
	})
	HasSystemctl = func() bool { return true }
	HostOS = "linux"
	Systemctl = func(...string) error { return nil }
	DaemonUnitPath = filepath.Join(t.TempDir(), "pmcluster.service")
	// Simulate a root-initialised install: only /root/.pmcluster exists.
	RootDataDir = filepath.Join(t.TempDir(), ".pmcluster")
	if err := os.MkdirAll(RootDataDir, 0o700); err != nil {
		t.Fatal(err)
	}

	var wrote = map[string][]byte{}
	WriteUnit = func(path string, data []byte, mode os.FileMode) error {
		wrote[path] = data
		return nil
	}

	// SUDO_USER set but with no data dir of their own → root fallback.
	os.Setenv("SUDO_USER", "sudoser")
	t.Setenv("HOME", "/tmp/nonexistent-sudoser-home")
	var out bytes.Buffer
	if err := EnsureDaemonRunning(&out, filepath.Join(t.TempDir(), "config.yaml")); err != nil {
		t.Fatalf("EnsureDaemonRunning: %v", err)
	}
	unit := wrote[DaemonUnitPath]
	if !bytes.Contains(unit, []byte("User=root")) {
		t.Errorf("unit must run as root when only /root/.pmcluster holds state:\n%s", unit)
	}
}

// TestEnsureDaemonRunning_SkipEnv verifies the PMCLUSTER_SKIP_DAEMON escape
// hatch: e2e harnesses start their own foreground daemon and must be able to
// suppress the systemd auto-start on Linux CI (a stray systemd daemon on the
// shared swarm would restore + re-snapshot control-plane state with a fresh
// timestamp and clobber a just-minted admin token).
func TestEnsureDaemonRunning_SkipEnv(t *testing.T) {
	t.Setenv("PMCLUSTER_SKIP_DAEMON", "1")
	HostOS = "linux"
	HasSystemctl = func() bool { return true }
	// If the guard is missing, this would try to write /etc/... via sudo and
	// fail on non-root dev hosts — a nil return + hint proves the skip fired.
	var out bytes.Buffer
	if err := EnsureDaemonRunning(&out, filepath.Join(t.TempDir(), "config.yaml")); err != nil {
		t.Fatalf("EnsureDaemonRunning with PMCLUSTER_SKIP_DAEMON=1: %v", err)
	}
	if !strings.Contains(out.String(), "PMCLUSTER_SKIP_DAEMON set") {
		t.Errorf("output missing skip hint:\n%s", out.String())
	}
}
