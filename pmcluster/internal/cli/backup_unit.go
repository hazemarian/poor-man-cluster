package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The backup agent is a host unit, not a Swarm service.
//
// Two reasons, both learned from the offen agent it replaces. A Swarm task can
// be rescheduled mid-run, which strands a half-written snapshot and drops the
// only record of what happened; and the work itself needs root — fsfreeze,
// lvcreate, mount — while the daemon deliberately runs as the operator user.
// So the privileged body is a one-shot unit and the daemon, which already
// writes its own unit, writes this one too. No new install path, no second
// daemon, and systemd — not the daemon — owns the schedule, so a restarting
// daemon cannot skip an hour.
//
// The unit is installed only where data lives: storage nodes. A node with no
// volumes has nothing to freeze, and installing it everywhere would make every
// worker carry a timer that can only no-op.

// renderBackupServiceUnit builds the oneshot unit that performs one run.
//
// ExecStartPre's flock is the whole concurrency control: a second trigger
// (the timer and an on-demand `backup create` landing together) exits
// immediately instead of interleaving two freezes on the same filesystem.
func renderBackupServiceUnit(exe, volumeRoot, archiveDir, stateDir string) string {
	return fmt.Sprintf(`[Unit]
Description=pmcluster volume snapshot backup (one run)
Documentation=https://github.com/hazemarian/poor-man-cluster
After=docker.service
Requires=docker.service
# A failed run must not be retried in a tight loop: the next tick is an hour
# away and starts from a clean slate.
StartLimitIntervalSec=300
StartLimitBurst=2

[Service]
Type=oneshot
# Root is required: fsfreeze, lvcreate, mount and umount. The daemon runs as
# the operator user and triggers this unit through systemd instead.
User=root
Environment=PMCLUSTER_VOLUME_ROOT=%s
Environment=PMCLUSTER_BACKUP_DIR=%s
Environment=PMCLUSTER_BACKUP_STATE=%s
ExecStartPre=/usr/bin/flock -n /run/pmcluster-backup.lock
ExecStart=%s backup-agent run
# The snapshot copy is I/O heavy and must never win a fight with an app.
Nice=10
IOSchedulingClass=best-effort
IOSchedulingPriority=7
TimeoutStartSec=3600

[Install]
WantedBy=multi-user.target
`, volumeRoot, archiveDir, stateDir, exe)
}

// renderBackupTimerUnit builds the hourly schedule.
//
// Persistent=true means a run missed because the node was down fires as soon
// as it comes back, instead of silently skipping an hour. AccuracySec=30s keeps
// the fleet from synchronising on the same second.
func renderBackupTimerUnit() string {
	return `[Unit]
Description=pmcluster volume snapshot backup (hourly)

[Timer]
OnCalendar=*-*-* *:00:00
AccuracySec=30s
Persistent=true
Unit=pmcluster-backup.service

[Install]
WantedBy=timers.target
`
}

// Overridable in tests, like the daemon's equivalents.
var (
	backupUnitPath      = "/etc/systemd/system/pmcluster-backup.service"
	backupTimerPath     = "/etc/systemd/system/pmcluster-backup.timer"
	backupAgentVolumeRd = defaultVolumeRoot
	backupAgentArchive  = "/var/stack/backup"
	backupAgentStateDir = "/var/lib/pmcluster-backup"
)

// ensureBackupAgentInstalled writes both units and enables the timer.
//
// It is deliberately quiet on non-Linux hosts and when systemctl is missing:
// the same binary is used on a laptop for `backup-agent drill`, and that must
// not require an init system.
func ensureBackupAgentInstalled(out io.Writer) error {
	if os.Getenv("PMCLUSTER_SKIP_BACKUP_AGENT") != "" {
		fmt.Fprintln(out, "  (PMCLUSTER_SKIP_BACKUP_AGENT set — the backup timer was not installed)")
		return nil
	}
	if hostOS != "linux" {
		fmt.Fprintln(out, "  (non-Linux host — the backup timer is a Linux systemd unit)")
		return nil
	}
	if !hasSystemctl() {
		fmt.Fprintln(out, "  (systemctl not found — run `pmcluster backup-agent run` from your own scheduler)")
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	if err := os.MkdirAll(backupAgentStateDir, 0o755); err != nil {
		// /var/lib needs root; the run itself will fail loudly if it cannot
		// write, so a permission problem here is worth reporting but not fatal
		// to the install.
		fmt.Fprintf(out, "  (could not create %s: %v)\n", backupAgentStateDir, err)
	}
	if err := writeUnitFn(backupUnitPath, []byte(renderBackupServiceUnit(exe, backupAgentVolumeRd, backupAgentArchive, backupAgentStateDir)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", backupUnitPath, err)
	}
	if err := writeUnitFn(backupTimerPath, []byte(renderBackupTimerUnit()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", backupTimerPath, err)
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", "pmcluster-backup.timer"}} {
		if err := systemctlFn(args...); err != nil {
			return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
		}
	}
	fmt.Fprintf(out, "  → installed %s + %s (hourly, root oneshot)\n", filepath.Base(backupUnitPath), filepath.Base(backupTimerPath))
	return nil
}
