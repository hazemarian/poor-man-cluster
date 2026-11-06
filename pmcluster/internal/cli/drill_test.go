package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The weekly timer has to exist, point at the drill, and not collide with the
// hourly backup run.
func TestDrillUnitsExistAndAreWeekly(t *testing.T) {
	svc := renderBackupDrillServiceUnit("/usr/local/bin/pmcluster", "/var/stack/data", "/var/stack/backup", "/var/lib/pmcluster-backup")
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
	timer := renderBackupDrillTimerUnit()
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

// --newest has to pick real artifacts, newest first, ignoring sidecars.
func TestNewestArtifactsSkipsSidecarsAndSortsByAge(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		ts := time.Now().Add(-age)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
	mk("backup-node1-2026-01-01T00-00-00-stack-a.tar.gz", 72*time.Hour)
	mk("backup-node1-2026-01-02T00-00-00-stack-a.tar.gz", 1*time.Hour)
	mk("backup-node1-2026-01-02T00-00-00-stack-a.tar.gz.json", 0) // sidecar, must be ignored
	mk("pmcluster-ctlplane-node1-2026-01-01T00-00-00.tar.gz", 2*time.Hour)
	mk("notes.txt", 0)

	got, err := newestArtifacts(dir, 2)
	if err != nil {
		t.Fatalf("newestArtifacts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d artifacts, want 2: %v", len(got), got)
	}
	// Newest first, and a control-plane archive is not a stack artifact: the
	// drill only drills what a restore would use.
	if !strings.Contains(got[0], "2026-01-02") || !strings.Contains(got[1], "2026-01-01") {
		t.Errorf("newest-first order wrong: %v", got)
	}
	for _, p := range got {
		if strings.Contains(p, "ctlplane") {
			t.Errorf("a control-plane archive was selected for a drill: %s", p)
		}
	}
	for _, p := range got {
		if strings.HasSuffix(p, ".json") {
			t.Errorf("a sidecar was selected as an artifact: %s", p)
		}
	}
}

// The status record is what makes "we drill weekly" verifiable.
func TestDrillStatusRecordsAndReadsBack(t *testing.T) {
	state := t.TempDir()
	rec := drillRecord{At: time.Now().UTC(), Archive: "backup-x-stack-a.tar.gz", Files: 12, Engine: "sqlite", Detail: "1 database(s) integrity_check ok", OK: true}
	if err := recordDrill(state, rec); err != nil {
		t.Fatalf("recordDrill: %v", err)
	}
	b, err := os.ReadFile(drillStatusPath(state))
	if err != nil {
		t.Fatalf("status file: %v", err)
	}
	if !strings.Contains(string(b), "backup-x-stack-a.tar.gz") || !strings.Contains(string(b), "\"ok\": true") {
		t.Errorf("status file does not carry the drill:\n%s", b)
	}
	if fi, _ := os.Stat(drillStatusPath(state)); fi != nil && fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("status file is readable by others: %v", fi.Mode())
	}
}

// A drill that cannot fit must refuse with a number, not fill the disk.
func TestEnsureDrillSpaceRefusesWhenTheArchiveCannotFit(t *testing.T) {
	dir := t.TempDir()
	if err := ensureDrillSpace(dir, 1<<40); err == nil {
		t.Fatal("a 1 TiB drill into a small temp dir was allowed")
	} else if !strings.Contains(err.Error(), "not enough space") {
		t.Errorf("error should say so plainly: %v", err)
	}
	if err := ensureDrillSpace(dir, 1024); err != nil {
		t.Errorf("a 1 KiB drill was refused: %v", err)
	}
}
