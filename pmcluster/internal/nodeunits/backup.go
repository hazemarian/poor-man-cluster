package nodeunits

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/manifest"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/storage"
)

// The backup agent is a host unit, not a Swarm service.
//
// Two reasons, both learned from the offen agent it replaces. A Swarm task can be
// rescheduled mid-run, which strands a half-written snapshot and drops the only
// record of what happened; and the work itself needs root — lvcreate, mount —
// while the daemon deliberately runs as the operator user. So the privileged body
// is a one-shot unit and the daemon, which already writes its own unit, writes
// this one too. No new install path, no second daemon, and systemd — not the
// daemon — owns the schedule, so a restarting daemon cannot skip an hour.
//
// The unit is installed only where data lives: storage nodes. A node with no
// volumes has nothing to freeze, and installing it everywhere would make every
// worker carry a timer that can only no-op.

// BackupEnvFile holds the ship target (and nothing else) for the hourly run.
// 0600 and root-owned: the unit is world-readable and the store credential must
// not be in it. A variable, not a constant, so a test asserts what is written
// instead of writing into /etc.
var BackupEnvFile = "/etc/pmcluster/backup.env"

// CanonicalAgentPath is where the platform installs its own binary; the timer must
// exec that, not whatever name the installer happened to be invoked as.
const CanonicalAgentPath = "/usr/local/bin/pmcluster"

// StoreServiceName is the Swarm service that carries the in-cluster store.
const StoreServiceName = "backup_seaweedfs"

// PlanCacheName is the node's cached copy of the backup plan. The daemon publishes
// it every few minutes and the agent prefers it over asking the leader, so a run
// never depends on the control plane being reachable — one name, or the writer and
// the reader drift and every hourly run plans from nothing.
const PlanCacheName = "plan.json"

// Paths and directories the units use. Variables, not constants, because every
// test that proves "a node that is not a storage node gets no timer" has to point
// them at a temp dir instead of /etc.
var (
	BackupUnitPath      = "/etc/systemd/system/pmcluster-backup.service"
	BackupTimerPath     = "/etc/systemd/system/pmcluster-backup.timer"
	BackupDrillUnitPath = "/etc/systemd/system/pmcluster-backup-drill.service"
	// PlatformUnitPath / PlatformTimerPath drive the CONTROL-PLANE export: data.db and the
	// encryption key, the restore point for losing every manager at once. Installed on every
	// manager (not only storage nodes) and guarded by --if-leader, so a follower's daily tick is
	// a no-op instead of a failed unit.
	PlatformUnitPath  = "/etc/systemd/system/pmcluster-platform.service"
	PlatformTimerPath = "/etc/systemd/system/pmcluster-platform.timer"
	BackupDrillTimer  = "/etc/systemd/system/pmcluster-backup-drill.timer"
	OffsiteUnitPath   = "/etc/systemd/system/pmcluster-backup-offsite.service"
	OffsiteTimerPath  = "/etc/systemd/system/pmcluster-backup-offsite.timer"
	AgentVolumeRoot   = manifest.DefaultVolumeRoot
	AgentArchiveDir   = "/var/stack/backup"
	AgentStateDir     = "/var/lib/pmcluster-backup"
)

// CanonicalAgentBinary picks the path the timer should exec.
//
// os.Executable returns whatever was invoked: installing the unit from
// /usr/local/bin/pmcluster.new wrote that name into ExecStart, so the timer kept
// running a renamed copy while every `pmcluster` command reported the new build.
// A stable install path wins whenever it exists.
func CanonicalAgentBinary(exe string, stat func(string) (os.FileInfo, error)) string {
	if _, err := stat(CanonicalAgentPath); err == nil {
		return CanonicalAgentPath
	}
	return exe
}

// WritePolicyFile writes the node's copy of the backup settings.
//
// This is how the control plane reaches a node without an endpoint: the installer
// reads the cluster settings (from this node's store when it has one) and writes a
// 0600 file the agent reads before every run. Re-running the installer is the
// refresh — one command, and it is the same command that installs the unit.
func WritePolicyFile(configPath string) error {
	pol := Policy{
		Enabled: EnvOrDefault("PMCLUSTER_BACKUP_ENABLED", "true"),
		Cron:    EnvOrDefault("PMCLUSTER_BACKUP_CRON", ""),
		Store:   EnvOrDefault("PMCLUSTER_BACKUP_STORE", ""),
	}
	pol.KeepLocal = EnvOrDefault("PMCLUSTER_BACKUP_KEEP_LOCAL", "")
	pol.S3.Endpoint = EnvOrDefault("PMCLUSTER_BACKUP_S3_ENDPOINT", "")
	pol.S3.Bucket = EnvOrDefault("PMCLUSTER_BACKUP_S3_BUCKET", "")
	pol.S3.Region = EnvOrDefault("PMCLUSTER_BACKUP_S3_REGION", "")
	pol.S3.AccessKey = EnvOrDefault("PMCLUSTER_BACKUP_S3_ACCESS_KEY", "")
	pol.S3.SecretKey = EnvOrDefault("PMCLUSTER_BACKUP_S3_SECRET_KEY", "")
	if days, err := strconv.Atoi(strings.TrimSpace(EnvOrDefault("PMCLUSTER_BACKUP_RETENTION_DAYS", ""))); err == nil && days > 0 {
		pol.RetentionDays = days
	}
	// Whatever the environment does not say, the control plane says. The offsite
	// target especially: it is a cluster setting the operator edits from the
	// console, and reading it only from env meant a node whose environment lacked
	// PMCLUSTER_BACKUP_S3_* wrote an EMPTY target into its policy and silently
	// shipped nothing offsite — a node fully configured for R2 in `cluster
	// settings`, whose every artifact stayed local.
	if st, err := openLocalStore(configPath); err == nil {
		ctx := context.Background()
		if pol.Cron == "" {
			pol.Cron = st.SettingDefault(ctx, cluster.SettingBackupCron(), "")
		}
		if pol.Store == "" {
			pol.Store = st.SettingDefault(ctx, cluster.SettingBackupStoreOn(), "")
		}
		// Retention belongs with the ship target: both are the answer to "how much
		// history does this cluster pay for". It used to stop at the setting, so
		// backup_retention_days was editable, accepted by the API, and read by
		// nothing — every node pruned at the flag default instead.
		if pol.RetentionDays == 0 {
			pol.RetentionDays = cluster.LoadBackupRetentionDays(ctx, st)
		}
		if pol.KeepLocal == "" {
			pol.KeepLocal = st.SettingDefault(ctx, cluster.SettingBackupKeepLocal(), "")
		}
		for _, f := range []struct {
			dst *string
			key string
		}{
			{&pol.S3.Endpoint, cluster.SettingBackupS3Endpoint()},
			{&pol.S3.Bucket, cluster.SettingBackupS3Bucket()},
			{&pol.S3.Region, cluster.SettingBackupS3Region()},
			{&pol.S3.AccessKey, cluster.SettingBackupS3AccessKey()},
			{&pol.S3.SecretKey, cluster.SettingBackupS3SecretKey()},
		} {
			if *f.dst == "" {
				*f.dst = st.SettingDefault(ctx, f.key, "")
			}
		}
		_ = st.Close()
	}
	if pol.Cron == "" {
		pol.Cron = cluster.DefaultBackupCron
	}
	// The in-cluster store is just another S3 target, recorded here so the agent
	// needs no environment at all.
	if pol.Store == "" || !IsOff(pol.Store) {
		if ak, sk := storeS3Keys(); ak != "" && sk != "" {
			pol.StoreS3 = S3Target{
				Endpoint: storeS3Endpoint(), Bucket: storeS3Bucket(),
				Region: "us-east-1", AccessKey: ak, SecretKey: sk,
			}
		}
	}
	b, err := json.MarshalIndent(pol, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(PolicyFile), 0o700); err != nil {
		return err
	}
	tmp := PolicyFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, PolicyFile)
}

// WriteEnvFile records the in-cluster store as an S3 target in the agent's
// environment.
//
// It used to write a WebDAV URL with the credentials embedded as userinfo. The
// store serves the S3 API on 8333 with the same Store.AccessKey/SecretKey, and
// SeaweedFS creates a bucket on first upload — so the WebDAV gateway, its port and
// its five-minute PUT timeout bought nothing but a second code path.
func WriteEnvFile() error {
	if err := os.MkdirAll(filepath.Dir(BackupEnvFile), 0o700); err != nil {
		return err
	}
	pol, _ := ReadPolicy(PolicyFile)
	if pol.Store != "" && IsOff(pol.Store) {
		return nil
	}
	ak, sk := storeS3Keys()
	if ak == "" || sk == "" {
		// No store deployed (or docker is not reachable): leave the file alone
		// rather than writing an endpoint that cannot authenticate.
		return nil
	}
	body := strings.Join([]string{
		"PMCLUSTER_BACKUP_STORE_S3_ENDPOINT=" + storeS3Endpoint(),
		"PMCLUSTER_BACKUP_STORE_S3_BUCKET=" + storeS3Bucket(),
		"PMCLUSTER_BACKUP_STORE_S3_REGION=us-east-1",
		"PMCLUSTER_BACKUP_STORE_S3_ACCESS_KEY=" + ak,
		"PMCLUSTER_BACKUP_STORE_S3_SECRET_KEY=" + sk,
	}, "\n") + "\n"
	temp := BackupEnvFile + ".tmp"
	if err := os.WriteFile(temp, []byte(body), 0o600); err != nil {
		return err
	}
	return os.Rename(temp, BackupEnvFile)
}

// storeS3Endpoint is the store's S3 API, reachable on the routing mesh.
func storeS3Endpoint() string {
	if v := EnvOrDefault("PMCLUSTER_BACKUP_STORE_S3_ENDPOINT", ""); v != "" {
		return v
	}
	return "http://127.0.0.1:8333"
}

// storeS3Bucket is where the store keeps artifacts.
func storeS3Bucket() string {
	if v := EnvOrDefault("PMCLUSTER_BACKUP_STORE_S3_BUCKET", ""); v != "" {
		return v
	}
	return "pmcluster-backups"
}

// storeS3Keys reads the store's own S3 identity from the running service — the
// same two variables SeaweedFS promotes to an admin identity.
func storeS3Keys() (string, string) {
	out, err := exec.Command("docker", "service", "inspect", StoreServiceName,
		"--format", "{{range .Spec.TaskTemplate.ContainerSpec.Env}}{{println .}}{{end}}").Output()
	if err != nil {
		return "", ""
	}
	var ak, sk string
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "AWS_ACCESS_KEY_ID="); ok {
			ak = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "AWS_SECRET_ACCESS_KEY="); ok {
			sk = strings.TrimSpace(v)
		}
	}
	return ak, sk
}

// RenderServiceUnit builds the oneshot unit that performs one run.
//
// The run lock lives in the agent, not here: a forced or single-stack run does not
// come through this unit, and the lock has to cover those too. The comment that
// used to justify flock in ExecStart follows, because the reasoning about WHERE the
// lock must sit is the part that was wrong.
//
// The lock is held for the whole run: a second trigger (the timer and an on-demand
// `backup create` landing together) finds the lock taken and exits instead of
// interleaving two freezes on the same filesystem. -n makes it fail immediately
// rather than queue, and -E 0 makes a skipped run exit 0 — nothing failed, a run
// was already in progress.
//
// It must NOT be an ExecStartPre: `flock -n <file>` with no command errors out
// ("bad file descriptor"), and even with a command the lock would be released
// before ExecStart ran, so the unit would not serialise anything at all.
func RenderServiceUnit(exe, volumeRoot, archiveDir, stateDir string) string {
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
# Root is required: lvcreate, mount and umount. The daemon runs as the operator
# user and triggers this unit through systemd instead.
User=root
Environment=PMCLUSTER_VOLUME_ROOT=%s
Environment=PMCLUSTER_BACKUP_DIR=%s
Environment=PMCLUSTER_BACKUP_STATE=%s
# The LVM coordinates: without them the agent cannot prove the volume root is
# its own and refuses to run (deliberately — a snapshot of the wrong volume is
# worse than no snapshot).
Environment=PMCLUSTER_BACKUP_VG=%s
Environment=PMCLUSTER_BACKUP_LV=%s
# Where to ship artifacts, and how long to keep them: the in-cluster store and
# the offsite bucket are both plain S3 targets now (endpoint, bucket, region,
# key, secret), written into this 0600 file by the installer. Credentials live
# here, never in the unit text: the unit is world-readable, and a backup store
# password does not belong in it.
EnvironmentFile=-%s
ExecStart=%s backup-agent run
# The snapshot copy is I/O heavy and must never win a fight with an app.
Nice=10
IOSchedulingClass=best-effort
IOSchedulingPriority=7
TimeoutStartSec=3600

[Install]
WantedBy=multi-user.target
`, volumeRoot, archiveDir, stateDir, storage.DefaultVG, storage.DefaultLV, BackupEnvFile, exe)
}

// RenderTimerUnit builds the hourly schedule.
//
// Persistent=true means a run missed because the node was down fires as soon as it
// comes back, instead of silently skipping an hour. AccuracySec=30s keeps the
// fleet from synchronising on the same second.
func RenderTimerUnit() string {
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

// RenderDrillServiceUnit builds the weekly restore drill.
//
// A backup nobody has restored is a hypothesis. The hourly run proves the artifact
// could be WRITTEN; this proves one can be READ — extracted, and its databases
// opened and asked a question. It runs on the node that holds the archive, uses the
// same scratch discipline as `backup-agent drill`, and is deliberately weekly: it
// competes with the apps for I/O and pool space.
func RenderDrillServiceUnit(exe, volumeRoot, archiveDir, stateDir string) string {
	return fmt.Sprintf(`[Unit]
Description=pmcluster restore drill (extract the newest artifact and verify its databases)
Documentation=https://github.com/hazemarian/poor-man-cluster
After=docker.service

[Service]
Type=oneshot
User=root
Environment=PMCLUSTER_VOLUME_ROOT=%s
Environment=PMCLUSTER_BACKUP_DIR=%s
Environment=PMCLUSTER_BACKUP_STATE=%s
EnvironmentFile=-%s
# --newest picks this node's newest artifact per stack; the space guard inside
# the command refuses rather than filling the disk a backup just freed.
ExecStart=%s backup-agent drill --newest 1
# The other half of the drill: the cluster state THIS node would promote on. It is
# mirrored by the mTLS pull and adopted in place, so a backup drill that proves an
# artifact restores still says nothing about whether a promotion from this node would
# work — which is exactly what the control-plane kit drill used to cover, and this is
# that check pointed at the mirror (version, age, rows, key). "-" keeps the step from
# masking a failed artifact drill; a worker fails it with a clear reason, which is
# honest on a node that holds no mirror by design.
ExecStart=-/bin/sh -c '%s cluster mirror verify'
# And the third artifact class: the control-plane export. Presence in the bucket has never been
# proof it opens (ADR 0008); this unpacks the newest one and proves the manifest, integrity_check
# and the key. --if-present makes a node that never exported say so instead of failing.
ExecStart=-/bin/sh -c '%s backup platform-verify --if-present'
Nice=15
IOSchedulingClass=best-effort
IOSchedulingPriority=7
# A drill that hangs holds a scratch copy of a database open; bound it.
TimeoutStartSec=1800

[Install]
WantedBy=multi-user.target
`, volumeRoot, archiveDir, stateDir, BackupEnvFile, exe, exe, exe)
}

// RenderDrillTimerUnit builds the weekly schedule: Sunday, 03:00, spread over 30
// minutes so a fleet does not all extract at once.
func RenderDrillTimerUnit() string {
	return `[Unit]
Description=pmcluster restore drill (weekly)

[Timer]
OnCalendar=Sun *-*-* 03:00:00
RandomizedDelaySec=30m
AccuracySec=1m
Persistent=true
Unit=pmcluster-backup-drill.service

[Install]
WantedBy=timers.target
`
}

// RenderOffsiteCheckServiceUnit builds the daily "is it still in the bucket?" check.
//
// The hourly run records where an artifact went at the time it shipped. That is
// intent, not presence: lifecycle rules expire objects, quotas fill, keys rotate, a
// bucket can be deleted. Nothing re-read the bucket after the fact, so "we have
// offsite backups" rested on a write that happened hours ago.
func RenderOffsiteCheckServiceUnit(exe, archiveDir, stateDir string) string {
	return fmt.Sprintf(`[Unit]
Description=pmcluster offsite check (are yesterday's artifacts still in the bucket?)
Documentation=https://github.com/hazemarian/poor-man-cluster

[Service]
Type=oneshot
User=root
Environment=PMCLUSTER_BACKUP_DIR=%s
Environment=PMCLUSTER_BACKUP_STATE=%s
EnvironmentFile=-%s
# Read-only against the bucket: list, compare, report. It ships nothing.
ExecStart=%s backup-agent verify-offsite --days 2
Nice=15
TimeoutStartSec=600

[Install]
WantedBy=multi-user.target
`, archiveDir, stateDir, BackupEnvFile, exe)
}

// RenderOffsiteCheckTimerUnit runs the check HOURLY, twenty minutes past the hour:
// the artifacts of the run that just happened are shipped and confirmed by then, so
// the status file never lags a day behind the thing it describes. A listing of the
// bucket is cheap; a stale "all present" is not.
func RenderOffsiteCheckTimerUnit() string {
	return `[Unit]
Description=pmcluster offsite check (daily)

[Timer]
OnCalendar=*:20:00
RandomizedDelaySec=20m
AccuracySec=1m
Persistent=true
Unit=pmcluster-backup-offsite.service

[Install]
WantedBy=timers.target
`
}

// IsStorageNode reports whether this host is meant to hold stack data.
//
// It is a question about the node's ROLE, not about its disk. Checking for a local
// LV is not enough: a worker joined with storage provisioned has one, and would then
// be handed an hourly root job that snapshots a volume holding nothing but a test
// stack — observed on production, where the worker also carried a 41 GB backup store
// on a 58 GB disk.
//
// A storage node is a manager (the fleet keeps stack data on managers) or a worker
// that was explicitly marked as one at join time, which sets PMCLUSTER_STORAGE_NODE
// in this unit's environment.
func IsStorageNode() (bool, string) {
	if os.Getenv("PMCLUSTER_STORAGE_NODE") == "1" {
		return true, ""
	}
	out, err := exec.Command("docker", "info", "--format", "{{.Swarm.ControlAvailable}}").Output()
	if err != nil {
		return false, "docker did not answer, so this node's swarm role is unknown"
	}
	if strings.TrimSpace(string(out)) == "true" {
		return true, ""
	}
	return false, "this node is a worker, and workers hold no stack data"
}

// StorageNodeFn is IsStorageNode, overridable for tests.
var StorageNodeFn = IsStorageNode

// RemoveBackupAgent takes the agent back off a node that should not have one. It is
// idempotent: a node that never had the units just gets two failed removes.
func RemoveBackupAgent(out io.Writer) {
	if HostOS == "linux" && HasSystemctl() {
		_ = Systemctl("disable", "--now", "pmcluster-backup.timer")
	}
	removed := false
	if HostOS == "linux" && HasSystemctl() {
		_ = Systemctl("disable", "--now", "pmcluster-backup-drill.timer")
	}
	for _, p := range []string{BackupTimerPath, BackupUnitPath, BackupDrillUnitPath, BackupDrillTimer} {
		if err := os.Remove(p); err == nil {
			removed = true
		}
	}
	// Unit files are not the only thing an install leaves behind: the admin drop-in
	// directory (and its drill twin) survives its unit and would sit there looking
	// like configuration for an agent this node no longer runs.
	for _, d := range []string{BackupUnitPath + ".d", BackupDrillUnitPath + ".d"} {
		if err := os.RemoveAll(d); err == nil {
			removed = true
		}
	}
	if removed {
		fmt.Fprintf(out, "  → removed %s and %s: this node is not a storage node\n",
			filepath.Base(BackupUnitPath), filepath.Base(BackupTimerPath))
		if HostOS == "linux" && HasSystemctl() {
			_ = Systemctl("daemon-reload")
		}
	}
}

// RenderPlatformServiceUnit is the daily control-plane export. It runs on every manager and
// decides for itself: `--if-leader` makes a follower exit 0 with one line, because a timer that
// fails on two of three managers every morning is a timer nobody keeps.
//
// The command ships the archive offsite itself — the export's whole value is NOT being on this
// host, so a unit that only wrote it to disk would produce a restore point that dies with the
// thing it restores.
func RenderPlatformServiceUnit(exe, archiveDir string) string {
	return fmt.Sprintf(`[Unit]
Description=pmcluster control-plane export (data.db + key — the restore point for every manager at once)
Documentation=https://github.com/hazemarian/poor-man-cluster
After=docker.service
Wants=docker.service

[Service]
Type=oneshot
User=root
Environment=PMCLUSTER_BACKUP_DIR=%s
Nice=10
# A follower holds no authoritative store, so it exports nothing and must not fail this unit.
ExecStart=%s backup platform --if-leader
`, archiveDir, exe)
}

// RenderPlatformTimerUnit runs the export once a day, away from the hourly stack backups.
// Persistent catches up after downtime: this is the only restore point for the whole control
// plane, so a missed night is worth making up.
func RenderPlatformTimerUnit() string {
	return `[Unit]
Description=Daily pmcluster control-plane export
Documentation=https://github.com/hazemarian/poor-man-cluster

[Timer]
OnCalendar=*-*-* 01:20:00
RandomizedDelaySec=20m
Persistent=true
Unit=pmcluster-platform.service

[Install]
WantedBy=timers.target
`
}

// EnsurePlatformExport installs the daily control-plane export. Separate from EnsureBackupAgent
// because the condition differs: an agent needs stack volumes (a storage node), an export needs
// the authoritative store (a manager). Gating one on the other left a manager that is not a
// storage node with no restore point at all.
func EnsurePlatformExport(out io.Writer, isManager bool) error {
	if !isManager {
		fmt.Fprintln(out, "  (not a manager — the control-plane export runs on managers only)")
		return nil
	}
	if os.Getenv("PMCLUSTER_SKIP_PLATFORM_EXPORT") != "" {
		fmt.Fprintln(out, "  (PMCLUSTER_SKIP_PLATFORM_EXPORT set — the export timer was not installed)")
		return nil
	}
	if HostOS != "linux" {
		fmt.Fprintln(out, "  (non-Linux host — the export timer is a Linux systemd unit)")
		return nil
	}
	if !HasSystemctl() {
		fmt.Fprintln(out, "  (systemctl not found — schedule `pmcluster backup platform` yourself)")
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	exe = CanonicalAgentBinary(exe, os.Stat)
	if err := WriteUnit(PlatformUnitPath, []byte(RenderPlatformServiceUnit(exe, AgentArchiveDir)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", PlatformUnitPath, err)
	}
	if err := WriteUnit(PlatformTimerPath, []byte(RenderPlatformTimerUnit()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", PlatformTimerPath, err)
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", "pmcluster-platform.timer"}} {
		if err := Systemctl(args...); err != nil {
			return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
		}
	}
	fmt.Fprintf(out, "  → installed %s + %s (daily control-plane export, 01:20)\n",
		filepath.Base(PlatformUnitPath), filepath.Base(PlatformTimerPath))
	return nil
}

// EnsureBackupAgent writes the units and enables the timers.
//
// It is deliberately quiet on non-Linux hosts and when systemctl is missing: the
// same binary is used on a laptop for `backup-agent drill`, and that must not
// require an init system.
func EnsureBackupAgent(out io.Writer, configPath string) error {
	// No volume, no backup. A worker does not hold stack data, so an agent here
	// would snapshot an empty directory forever.
	if ok, why := StorageNodeFn(); !ok {
		fmt.Fprintf(out, "  (not a storage node — %s; the backup agent was not installed)\n", why)
		RemoveBackupAgent(out)
		return nil
	}
	if os.Getenv("PMCLUSTER_SKIP_BACKUP_AGENT") != "" {
		fmt.Fprintln(out, "  (PMCLUSTER_SKIP_BACKUP_AGENT set — the backup timer was not installed)")
		return nil
	}
	if HostOS != "linux" {
		fmt.Fprintln(out, "  (non-Linux host — the backup timer is a Linux systemd unit)")
		return nil
	}
	if !HasSystemctl() {
		fmt.Fprintln(out, "  (systemctl not found — run `pmcluster backup-agent run` from your own scheduler)")
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	exe = CanonicalAgentBinary(exe, os.Stat)
	if err := os.MkdirAll(AgentStateDir, 0o755); err != nil {
		// /var/lib needs root; the run itself will fail loudly if it cannot write,
		// so a permission problem here is worth reporting but not fatal to the
		// install.
		fmt.Fprintf(out, "  (could not create %s: %v)\n", AgentStateDir, err)
	}
	if err := WritePolicyFile(configPath); err != nil {
		fmt.Fprintf(out, "  (could not write %s: %v — the agent will use this node's stored settings)\n", PolicyFile, err)
	}
	if err := WriteEnvFile(); err != nil {
		fmt.Fprintf(out, "  (could not write %s: %v — artifacts will stay local)\n", BackupEnvFile, err)
	}
	if err := WriteUnit(BackupUnitPath, []byte(RenderServiceUnit(exe, AgentVolumeRoot, AgentArchiveDir, AgentStateDir)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", BackupUnitPath, err)
	}
	if err := WriteUnit(BackupTimerPath, []byte(RenderTimerUnit()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", BackupTimerPath, err)
	}
	if err := WriteUnit(BackupDrillUnitPath, []byte(RenderDrillServiceUnit(exe, AgentVolumeRoot, AgentArchiveDir, AgentStateDir)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", BackupDrillUnitPath, err)
	}
	if err := WriteUnit(BackupDrillTimer, []byte(RenderDrillTimerUnit()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", BackupDrillTimer, err)
	}
	if err := WriteUnit(OffsiteUnitPath, []byte(RenderOffsiteCheckServiceUnit(exe, AgentArchiveDir, AgentStateDir)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", OffsiteUnitPath, err)
	}
	if err := WriteUnit(OffsiteTimerPath, []byte(RenderOffsiteCheckTimerUnit()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", OffsiteTimerPath, err)
	}
	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", "--now", "pmcluster-backup.timer"},
		{"enable", "--now", "pmcluster-backup-drill.timer"},
		{"enable", "--now", "pmcluster-backup-offsite.timer"},
	} {
		if err := Systemctl(args...); err != nil {
			return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
		}
	}
	fmt.Fprintf(out, "  → installed %s + %s (hourly, root oneshot)\n", filepath.Base(BackupUnitPath), filepath.Base(BackupTimerPath))
	fmt.Fprintf(out, "  → installed %s + %s (weekly restore drill, Sunday 03:00)\n", filepath.Base(BackupDrillUnitPath), filepath.Base(BackupDrillTimer))
	fmt.Fprintf(out, "  → installed %s + %s (hourly offsite check, twenty past the hour)\n", filepath.Base(OffsiteUnitPath), filepath.Base(OffsiteTimerPath))
	return nil
}
