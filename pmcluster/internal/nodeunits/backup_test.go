package nodeunits

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderBackupServiceUnit(t *testing.T) {
	u := RenderServiceUnit("/usr/local/bin/pmcluster", "/var/stack/data", "/var/stack/backup", "/var/lib/pmcluster-backup")
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
	u := RenderTimerUnit()
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
// isolatePaths points the installed files at a temp dir: a test that writes the
// agent's real paths is a test that fails on a locked-down box, and passes for the
// wrong reason as root.
func isolatePaths(t *testing.T) (cfgPath string) {
	t.Helper()
	dir := t.TempDir()
	oldPolicy, oldEnv := PolicyFile, BackupEnvFile
	PolicyFile = filepath.Join(dir, "backup.conf")
	BackupEnvFile = filepath.Join(dir, "backup.env")
	t.Cleanup(func() { PolicyFile, BackupEnvFile = oldPolicy, oldEnv })
	return filepath.Join(dir, "config.yaml")
}

func storageNodeForTest(t *testing.T, is bool) {
	t.Helper()
	prev := StorageNodeFn
	StorageNodeFn = func() (bool, string) {
		if is {
			return true, ""
		}
		return false, "/var/stack/data is mounted from nothing"
	}
	t.Cleanup(func() { StorageNodeFn = prev })
}

func TestEnsureBackupAgentInstalled(t *testing.T) {
	cfgPath := isolatePaths(t)
	storageNodeForTest(t, true)
	dir := t.TempDir()
	oldUnit, oldTimer, oldDrillUnit, oldDrillTimer := BackupUnitPath, BackupTimerPath, BackupDrillUnitPath, BackupDrillTimer
	oldOffsiteUnit, oldOffsiteTimer := OffsiteUnitPath, OffsiteTimerPath
	oldSys, oldHostOS, oldHasSys, oldWrite := Systemctl, HostOS, HasSystemctl, WriteUnit
	oldState := AgentStateDir
	defer func() {
		BackupUnitPath, BackupTimerPath = oldUnit, oldTimer
		BackupDrillUnitPath, BackupDrillTimer = oldDrillUnit, oldDrillTimer
		OffsiteUnitPath, OffsiteTimerPath = oldOffsiteUnit, oldOffsiteTimer
		Systemctl, HostOS, HasSystemctl, WriteUnit = oldSys, oldHostOS, oldHasSys, oldWrite
		AgentStateDir = oldState
	}()

	BackupUnitPath = filepath.Join(dir, "pmcluster-backup.service")
	BackupTimerPath = filepath.Join(dir, "pmcluster-backup.timer")
	BackupDrillUnitPath = filepath.Join(dir, "pmcluster-backup-drill.service")
	BackupDrillTimer = filepath.Join(dir, "pmcluster-backup-drill.timer")
	OffsiteUnitPath = filepath.Join(dir, "pmcluster-backup-offsite.service")
	OffsiteTimerPath = filepath.Join(dir, "pmcluster-backup-offsite.timer")
	AgentStateDir = filepath.Join(dir, "state")
	WriteUnit = os.WriteFile
	HostOS = "linux"
	HasSystemctl = func() bool { return true }

	var calls []string
	Systemctl = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}

	var out bytes.Buffer
	if err := EnsureBackupAgent(&out, cfgPath); err != nil {
		t.Fatalf("EnsureBackupAgent: %v", err)
	}
	for _, p := range []string{BackupUnitPath, BackupTimerPath, BackupDrillUnitPath, BackupDrillTimer, OffsiteUnitPath, OffsiteTimerPath} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s not written: %v", p, err)
		}
		if len(b) == 0 {
			t.Fatalf("%s is empty", p)
		}
	}
	want := []string{"daemon-reload", "enable --now pmcluster-backup.timer", "enable --now pmcluster-backup-drill.timer", "enable --now pmcluster-backup-offsite.timer"}
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
	cfgPath := isolatePaths(t)
	storageNodeForTest(t, true)
	oldHostOS, oldHasSys, oldWrite := HostOS, HasSystemctl, WriteUnit
	defer func() { HostOS, HasSystemctl, WriteUnit = oldHostOS, oldHasSys, oldWrite }()
	HostOS = "darwin"
	HasSystemctl = func() bool { return true }
	WriteUnit = func(string, []byte, os.FileMode) error {
		t.Fatal("wrote a unit on a non-Linux host")
		return nil
	}
	var out bytes.Buffer
	if err := EnsureBackupAgent(&out, cfgPath); err != nil {
		t.Fatalf("want nil on non-Linux, got %v", err)
	}
	if !strings.Contains(out.String(), "systemd") {
		t.Errorf("output does not explain why nothing happened: %q", out.String())
	}
}

func TestEnsureBackupAgentInstalledHonoursSkipEnv(t *testing.T) {
	cfgPath := isolatePaths(t)
	storageNodeForTest(t, true)
	t.Setenv("PMCLUSTER_SKIP_BACKUP_AGENT", "1")
	oldWrite := WriteUnit
	defer func() { WriteUnit = oldWrite }()
	WriteUnit = func(string, []byte, os.FileMode) error {
		t.Fatal("wrote a unit despite PMCLUSTER_SKIP_BACKUP_AGENT")
		return nil
	}
	var out bytes.Buffer
	if err := EnsureBackupAgent(&out, cfgPath); err != nil {
		t.Fatalf("want nil with the skip env, got %v", err)
	}
}

// A worker holds no stack data. Installing an hourly root job that snapshots a
// volume that is not there buys nothing — and on production it meant a node with no
// storage ran a backup agent while its pool sat unused.
func TestEnsureBackupAgentIsNotInstalledOnANonStorageNode(t *testing.T) {
	cfgPath := isolatePaths(t)
	dir := t.TempDir()
	BackupUnitPath = filepath.Join(dir, "pmcluster-backup.service")
	BackupTimerPath = filepath.Join(dir, "pmcluster-backup.timer")
	BackupDrillUnitPath = filepath.Join(dir, "pmcluster-backup-drill.service")
	BackupDrillTimer = filepath.Join(dir, "pmcluster-backup-drill.timer")
	OffsiteUnitPath = filepath.Join(dir, "pmcluster-backup-offsite.service")
	OffsiteTimerPath = filepath.Join(dir, "pmcluster-backup-offsite.timer")
	storageNodeForTest(t, false)

	var out bytes.Buffer
	if err := EnsureBackupAgent(&out, cfgPath); err != nil {
		t.Fatalf("a non-storage node must be a clean no-op, got: %v", err)
	}
	if _, err := os.Stat(BackupTimerPath); !os.IsNotExist(err) {
		t.Fatal("the timer was installed on a node with no volume to snapshot")
	}
	if !strings.Contains(out.String(), "not a storage node") {
		t.Fatalf("the operator was not told why: %q", out.String())
	}
}

// And if one was installed before the node stopped being a storage node, it comes
// back off rather than lingering.
func TestEnsureBackupAgentRemovesLeftoverUnits(t *testing.T) {
	cfgPath := isolatePaths(t)
	dir := t.TempDir()
	BackupUnitPath = filepath.Join(dir, "pmcluster-backup.service")
	BackupTimerPath = filepath.Join(dir, "pmcluster-backup.timer")
	BackupDrillUnitPath = filepath.Join(dir, "pmcluster-backup-drill.service")
	BackupDrillTimer = filepath.Join(dir, "pmcluster-backup-drill.timer")
	OffsiteUnitPath = filepath.Join(dir, "pmcluster-backup-offsite.service")
	OffsiteTimerPath = filepath.Join(dir, "pmcluster-backup-offsite.timer")
	for _, p := range []string{BackupUnitPath, BackupTimerPath, BackupDrillUnitPath, BackupDrillTimer, OffsiteUnitPath, OffsiteTimerPath} {
		if err := os.WriteFile(p, []byte("[Unit]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	storageNodeForTest(t, false)

	var out bytes.Buffer
	if err := EnsureBackupAgent(&out, cfgPath); err != nil {
		t.Fatalf("EnsureBackupAgent: %v", err)
	}
	if _, err := os.Stat(BackupTimerPath); !os.IsNotExist(err) {
		t.Fatal("a leftover timer survived on a node that is no longer a storage node")
	}
}

// The weekly timer has to exist, point at the drill, and not collide with the
// hourly backup run.
func TestDrillUnitsExistAndAreWeekly(t *testing.T) {
	svc := RenderDrillServiceUnit("/usr/local/bin/pmcluster", "/var/stack/data", "/var/stack/backup", "/var/lib/pmcluster-backup")
	for _, want := range []string{
		"User=root",
		"ExecStart=/usr/local/bin/pmcluster backup-agent drill --newest 1",
		"PMCLUSTER_BACKUP_DIR=/var/stack/backup",
		"TimeoutStartSec=",
	} {
		if !strings.Contains(svc, want) {
			t.Errorf("drill service is missing %q:\n%s", want, svc)
		}
	}
	timer := RenderDrillTimerUnit()
	for _, want := range []string{"OnCalendar=Sun", "Unit=pmcluster-backup-drill.service", "Persistent=true"} {
		if !strings.Contains(timer, want) {
			t.Errorf("drill timer is missing %q:\n%s", want, timer)
		}
	}
	// Weekly, not hourly: a drill costs the I/O of a restore and must not run
	// alongside the backup it is testing.
	if strings.Contains(timer, "*-*-* *:00:00") {
		t.Errorf("drill timer looks hourly:\n%s", timer)
	}
}

