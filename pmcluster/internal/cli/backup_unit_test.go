package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderBackupServiceUnit(t *testing.T) {
	u := renderBackupServiceUnit("/usr/local/bin/pmcluster", "/var/stack/data", "/var/stack/backup", "/var/lib/pmcluster-backup")
	for _, want := range []string{
		"Type=oneshot",
		"User=root",
		// The lock wraps the run (held for its whole duration) and a skipped
		// run is not an error — see the deployment test that caught the
		// ExecStartPre version failing outright.
		"ExecStart=/usr/local/bin/pmcluster backup-agent run",
		"Environment=PMCLUSTER_VOLUME_ROOT=/var/stack/data",
		"Environment=PMCLUSTER_BACKUP_DIR=/var/stack/backup",
		"TimeoutStartSec=3600",
		// The copy must lose to the applications it is archiving.
		"Nice=10",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("service unit missing %q\n%s", want, u)
		}
	}
	// A restart policy on a oneshot would loop; assert we did not add one.
	if strings.Contains(u, "Restart=") {
		t.Errorf("oneshot unit must not carry Restart=\n%s", u)
	}
	// The lock must never live in the unit at all: a forced or single-stack run
	// never comes through it, which is how two runs once froze one filesystem.
	if strings.Contains(u, "flock") {
		t.Fatalf("the unit must not take the run lock; the agent owns it:\n%s", u)
	}
	// The lock must never move back into ExecStartPre: with no command flock
	// fails, and with one it releases before the run starts — either way the
	// unit stops serialising, which is the whole property.
	if strings.Contains(u, "ExecStartPre=/usr/bin/flock") {
		t.Errorf("flock must wrap ExecStart, not ExecStartPre\n%s", u)
	}
}

func TestRenderBackupTimerUnit(t *testing.T) {
	u := renderBackupTimerUnit()
	for _, want := range []string{
		"OnCalendar=*-*-* *:00:00",
		"Unit=pmcluster-backup.service",
		// A missed hour must fire on the next boot instead of vanishing.
		"Persistent=true",
		"WantedBy=timers.target",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("timer unit missing %q\n%s", want, u)
		}
	}
}

// TestEnsureBackupAgentInstalled covers the install sequence: both files
// written, daemon-reload then enable --now, using the same seams the daemon
// installer uses so no test touches a real init system.
func storageNodeForTest(t *testing.T, is bool) {
	t.Helper()
	prev := storageNodeFn
	storageNodeFn = func() (bool, string) {
		if is {
			return true, ""
		}
		return false, "/var/stack/data is mounted from nothing"
	}
	t.Cleanup(func() { storageNodeFn = prev })
}

func TestEnsureBackupAgentInstalled(t *testing.T) {
	storageNodeForTest(t, true)
	dir := t.TempDir()
	oldUnit, oldTimer := backupUnitPath, backupTimerPath
	oldSys, oldHostOS, oldHasSys, oldWrite := systemctlFn, hostOS, hasSystemctl, writeUnitFn
	oldState := backupAgentStateDir
	defer func() {
		backupUnitPath, backupTimerPath = oldUnit, oldTimer
		systemctlFn, hostOS, hasSystemctl, writeUnitFn = oldSys, oldHostOS, oldHasSys, oldWrite
		backupAgentStateDir = oldState
	}()

	backupUnitPath = filepath.Join(dir, "pmcluster-backup.service")
	backupTimerPath = filepath.Join(dir, "pmcluster-backup.timer")
	backupAgentStateDir = filepath.Join(dir, "state")
	writeUnitFn = os.WriteFile
	hostOS = "linux"
	hasSystemctl = func() bool { return true }

	var calls []string
	systemctlFn = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}

	var out bytes.Buffer
	if err := ensureBackupAgentInstalled(&out); err != nil {
		t.Fatalf("ensureBackupAgentInstalled: %v", err)
	}
	for _, p := range []string{backupUnitPath, backupTimerPath} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s not written: %v", p, err)
		}
		if len(b) == 0 {
			t.Fatalf("%s is empty", p)
		}
	}
	want := []string{"daemon-reload", "enable --now pmcluster-backup.timer"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("systemctl calls = %v, want %v", calls, want)
	}
	if !strings.Contains(out.String(), "hourly") {
		t.Errorf("install message does not state the schedule: %q", out.String())
	}
}

// A laptop runs the binary for `backup-agent drill`; installing a timer there
// must be impossible rather than merely unlikely.
func TestEnsureBackupAgentInstalledSkipsNonLinux(t *testing.T) {
	storageNodeForTest(t, true)
	oldHostOS, oldHasSys, oldWrite := hostOS, hasSystemctl, writeUnitFn
	defer func() { hostOS, hasSystemctl, writeUnitFn = oldHostOS, oldHasSys, oldWrite }()
	hostOS = "darwin"
	hasSystemctl = func() bool { return true }
	writeUnitFn = func(string, []byte, os.FileMode) error {
		t.Fatal("wrote a unit on a non-Linux host")
		return nil
	}
	var out bytes.Buffer
	if err := ensureBackupAgentInstalled(&out); err != nil {
		t.Fatalf("want nil on non-Linux, got %v", err)
	}
	if !strings.Contains(out.String(), "systemd") {
		t.Errorf("output does not explain why nothing happened: %q", out.String())
	}
}

func TestEnsureBackupAgentInstalledHonoursSkipEnv(t *testing.T) {
	storageNodeForTest(t, true)
	t.Setenv("PMCLUSTER_SKIP_BACKUP_AGENT", "1")
	oldWrite := writeUnitFn
	defer func() { writeUnitFn = oldWrite }()
	writeUnitFn = func(string, []byte, os.FileMode) error {
		t.Fatal("wrote a unit despite PMCLUSTER_SKIP_BACKUP_AGENT")
		return nil
	}
	var out bytes.Buffer
	if err := ensureBackupAgentInstalled(&out); err != nil {
		t.Fatalf("want nil with the skip env, got %v", err)
	}
}

// A worker holds no stack data. Installing an hourly root job that snapshots a
// volume that is not there buys nothing — and on production it meant a node with no
// storage ran a backup agent while its pool sat unused.
func TestEnsureBackupAgentIsNotInstalledOnANonStorageNode(t *testing.T) {
	dir := t.TempDir()
	backupUnitPath = filepath.Join(dir, "pmcluster-backup.service")
	backupTimerPath = filepath.Join(dir, "pmcluster-backup.timer")
	storageNodeForTest(t, false)

	var out bytes.Buffer
	if err := ensureBackupAgentInstalled(&out); err != nil {
		t.Fatalf("a non-storage node must be a clean no-op, got: %v", err)
	}
	if _, err := os.Stat(backupTimerPath); !os.IsNotExist(err) {
		t.Fatal("the timer was installed on a node with no volume to snapshot")
	}
	if !strings.Contains(out.String(), "not a storage node") {
		t.Fatalf("the operator was not told why: %q", out.String())
	}
}

// And if one was installed before the node stopped being a storage node, it comes
// back off rather than lingering.
func TestEnsureBackupAgentRemovesLeftoverUnits(t *testing.T) {
	dir := t.TempDir()
	backupUnitPath = filepath.Join(dir, "pmcluster-backup.service")
	backupTimerPath = filepath.Join(dir, "pmcluster-backup.timer")
	for _, p := range []string{backupUnitPath, backupTimerPath} {
		if err := os.WriteFile(p, []byte("[Unit]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	storageNodeForTest(t, false)

	var out bytes.Buffer
	if err := ensureBackupAgentInstalled(&out); err != nil {
		t.Fatalf("ensureBackupAgentInstalled: %v", err)
	}
	if _, err := os.Stat(backupTimerPath); !os.IsNotExist(err) {
		t.Fatal("a leftover timer survived on a node that is no longer a storage node")
	}
}
