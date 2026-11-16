package nodeunits

import (
	"strings"
	"testing"
)

// TestRenderPlatformServiceUnit pins the three properties that make the daily export safe to put
// on every manager: it is a oneshot, it asks before it writes (--if-leader), and it targets the
// archive dir the rest of the backup machinery uses. A unit that skipped --if-leader would fail
// on two managers out of three every morning, which is how a timer trains its operator to ignore
// it — the whole reason this exists is that the restore point was manual and forgotten.
func TestRenderPlatformServiceUnit(t *testing.T) {
	u := RenderPlatformServiceUnit("/usr/local/bin/pmcluster", "/var/stack/backup")
	for _, want := range []string{
		"Type=oneshot",
		"User=root",
		"ExecStart=/usr/local/bin/pmcluster backup platform --if-leader",
		"Environment=PMCLUSTER_BACKUP_DIR=/var/stack/backup",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("the platform service unit is missing %q:\n%s", want, u)
		}
	}
	// It must not carry the volume root: it exports the store, not stack data.
	if strings.Contains(u, "PMCLUSTER_VOLUME_ROOT") {
		t.Error("the platform export unit sets PMCLUSTER_VOLUME_ROOT — it touches no stack volume")
	}
}

func TestRenderPlatformTimerUnit(t *testing.T) {
	u := RenderPlatformTimerUnit()
	for _, want := range []string{
		"OnCalendar=*-*-* 01:20:00",
		"Persistent=true",
		"Unit=pmcluster-platform.service",
		"WantedBy=timers.target",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("the platform timer is missing %q:\n%s", want, u)
		}
	}
	// Daily, and not on the hour the hourly stack backup fires.
	if strings.Contains(u, "OnCalendar=*-*-* *:") {
		t.Error("the platform timer is set to run more than once a day")
	}
	if strings.Contains(u, "OnCalendar=*-*-* 01:00:00") {
		t.Error("the platform export runs at 01:00, the same minute as the hourly stack backup")
	}
}

// TestEnsurePlatformExportSkipsANonManager: a worker has no authoritative store, so installing a
// timer there would only produce a failing unit every night.
func TestEnsurePlatformExportSkipsANonManager(t *testing.T) {
	var out strings.Builder
	if err := EnsurePlatformExport(&out, false); err != nil {
		t.Fatalf("EnsurePlatformExport(worker): %v", err)
	}
	if !strings.Contains(out.String(), "not a manager") {
		t.Errorf("a worker was not told why nothing was installed: %q", out.String())
	}
}

// TestDrillVerifiesThePlatformExport: the weekly drill must cover all three artifact classes, and
// the control-plane export is the one that was only ever checked for presence in a bucket.
func TestDrillVerifiesThePlatformExport(t *testing.T) {
	u := RenderDrillServiceUnit("/usr/local/bin/pmcluster", "/var/stack/data", "/var/stack/backup", "/var/lib/pmcluster-backup")
	if !strings.Contains(u, "backup platform-verify --if-present") {
		t.Fatalf("the weekly drill does not verify the control-plane export:\n%s", u)
	}
	if !strings.Contains(u, "backup-agent drill --newest 1") {
		t.Error("the drill lost its stack-artifact step")
	}
	// - keeps one failing step from masking the others.
	if strings.Contains(u, "\nExecStart=/usr/local/bin/pmcluster backup platform-verify") {
		t.Error("the platform-verify step is not prefixed with -, so it would mask the other steps' status")
	}
}

func TestEnsurePlatformExportWritesUnits(t *testing.T) {
	if HostOS != "linux" || !HasSystemctl() {
		t.Skip("systemd units are Linux-only")
	}
	// Writing units and running systemctl needs root and a real /etc; this asserts the RENDERED
	// content instead, which is the part a regression would change.
	svc := RenderPlatformServiceUnit("/usr/local/bin/pmcluster", AgentArchiveDir)
	if !strings.Contains(svc, "pmcluster backup platform --if-leader") {
		t.Fatal("the rendered service unit does not invoke the export")
	}
	if !strings.Contains(RenderPlatformTimerUnit(), "pmcluster-platform.service") {
		t.Fatal("the timer does not point at the export service")
	}
}
