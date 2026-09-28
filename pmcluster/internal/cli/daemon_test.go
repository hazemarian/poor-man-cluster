package cli

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
	if err := writeFileSudo(p, []byte("hello"), 0o644); err != nil {
		t.Fatalf("writeFileSudo: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil || string(got) != "hello" {
		t.Fatalf("read back = %q, %v; want hello", got, err)
	}
}

func TestEnsureDaemonRunningWritesUnitAndStarts(t *testing.T) {
	// Save + restore the overridable globals.
	oldPath := daemonUnitPath
	oldSys := systemctlFn
	oldHostOS := hostOS
	oldHasSys := hasSystemctl
	oldWrite := writeUnitFn
	oldPMCLUSTER_USER, hadPM := os.LookupEnv("PMCLUSTER_USER")
	oldSUDO_USER, hadSUDO := os.LookupEnv("SUDO_USER")
	defer func() {
		daemonUnitPath = oldPath
		systemctlFn = oldSys
		hostOS = oldHostOS
		hasSystemctl = oldHasSys
		writeUnitFn = oldWrite
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
	hostOS = "linux"
	hasSystemctl = func() bool { return true }
	writeUnitFn = os.WriteFile
	os.Setenv("PMCLUSTER_USER", "testuser")

	unitPath := filepath.Join(t.TempDir(), "pmcluster.service")
	daemonUnitPath = unitPath
	callLog := filepath.Join(t.TempDir(), "calls.log")
	systemctlFn = fakeSystemctl(t, callLog)

	var out bytes.Buffer
	if err := ensureDaemonRunning(&out); err != nil {
		t.Fatalf("ensureDaemonRunning: %v", err)
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
	oldPath := daemonUnitPath
	oldSys := systemctlFn
	oldHostOS := hostOS
	oldHasSys := hasSystemctl
	oldWrite := writeUnitFn
	oldPM, hadPM := os.LookupEnv("PMCLUSTER_USER")
	defer func() {
		daemonUnitPath = oldPath
		systemctlFn = oldSys
		hostOS = oldHostOS
		hasSystemctl = oldHasSys
		writeUnitFn = oldWrite
		if hadPM {
			os.Setenv("PMCLUSTER_USER", oldPM)
		} else {
			os.Unsetenv("PMCLUSTER_USER")
		}
	}()
	hostOS = "linux"
	hasSystemctl = func() bool { return true }
	writeUnitFn = os.WriteFile
	os.Setenv("PMCLUSTER_USER", "testuser")

	unitPath := filepath.Join(t.TempDir(), "pmcluster.service")
	daemonUnitPath = unitPath
	systemctlFn = func(...string) error { return nil }

	var out1, out2 bytes.Buffer
	if err := ensureDaemonRunning(&out1); err != nil {
		t.Fatal(err)
	}
	// Second run: unit already matches → must NOT report "installed".
	if err := ensureDaemonRunning(&out2); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out2.String(), "installed systemd unit") {
		t.Errorf("second run should not re-install the unit:\n%s", out2.String())
	}
}

func TestSystemctlSudoErrorWrapping(t *testing.T) {
	err := systemctlSudo("bogus-command-xyz")
	if err == nil {
		t.Skip("systemctl accepted bogus command (unusual) — nothing to assert")
	}
	if !strings.Contains(err.Error(), "bogus-command-xyz") {
		t.Errorf("error should mention the command, got: %v", err)
	}
}
