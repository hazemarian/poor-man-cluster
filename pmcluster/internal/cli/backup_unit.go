package cli

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
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/storage"
)

// The backup agent is a host unit, not a Swarm service.
//
// Two reasons, both learned from the offen agent it replaces. A Swarm task can
// be rescheduled mid-run, which strands a half-written snapshot and drops the
// only record of what happened; and the work itself needs root — lvcreate,
// mount — while the daemon deliberately runs as the operator user.
// So the privileged body is a one-shot unit and the daemon, which already
// writes its own unit, writes this one too. No new install path, no second
// daemon, and systemd — not the daemon — owns the schedule, so a restarting
// daemon cannot skip an hour.
//
// The unit is installed only where data lives: storage nodes. A node with no
// volumes has nothing to freeze, and installing it everywhere would make every
// worker carry a timer that can only no-op.

// backupEnvFile holds the ship target (and nothing else) for the hourly run.
// 0600 and root-owned: the unit is world-readable and the store credential must
// not be in it.
const backupEnvFile = "/etc/pmcluster/backup.env"

// canonicalAgentPath is where the platform installs its own binary; the timer
// must exec that, not whatever name the installer happened to be invoked as.
const canonicalAgentPath = "/usr/local/bin/pmcluster"

// canonicalAgentBinary picks the path the timer should exec.
//
// os.Executable returns whatever was invoked: installing the unit from
// /usr/local/bin/pmcluster.new wrote that name into ExecStart, so the timer kept
// running a renamed copy while every `pmcluster` command reported the new build.
// A stable install path wins whenever it exists.
func canonicalAgentBinary(exe string, stat func(string) (os.FileInfo, error)) string {
	if _, err := stat(canonicalAgentPath); err == nil {
		return canonicalAgentPath
	}
	return exe
}

// writeBackupPolicyFile writes the node's copy of the backup settings.
//
// This is how the control plane reaches a node without an endpoint: the installer
// reads the cluster settings (from this node's store when it has one) and writes
// a 0600 file the agent reads before every run. Re-running the installer is the
// refresh — one command, and it is the same command that installs the unit.
func writeBackupPolicyFile() error {
	pol := backupPolicy{
		Enabled: envOr("PMCLUSTER_BACKUP_ENABLED", "true"),
		Cron:    envOr("PMCLUSTER_BACKUP_CRON", ""),
		Store:   envOr("PMCLUSTER_BACKUP_STORE", ""),
	}
	pol.S3.Endpoint = envOr("PMCLUSTER_BACKUP_S3_ENDPOINT", "")
	pol.S3.Bucket = envOr("PMCLUSTER_BACKUP_S3_BUCKET", "")
	pol.S3.Region = envOr("PMCLUSTER_BACKUP_S3_REGION", "")
	pol.S3.AccessKey = envOr("PMCLUSTER_BACKUP_S3_ACCESS_KEY", "")
	pol.S3.SecretKey = envOr("PMCLUSTER_BACKUP_S3_SECRET_KEY", "")
	if days, err := strconv.Atoi(strings.TrimSpace(envOr("PMCLUSTER_BACKUP_RETENTION_DAYS", ""))); err == nil && days > 0 {
		pol.RetentionDays = days
	}
	// Whatever the environment does not say, the control plane says. The offsite
	// target especially: it is a cluster setting the operator edits from the
	// console, and reading it only from env meant a node whose environment lacked
	// PMCLUSTER_BACKUP_S3_* wrote an EMPTY target into its policy and silently
	// shipped nothing offsite — a node fully configured for R2 in `cluster
	// settings`, whose every artifact stayed local.
	if st, _, err := openStore(); err == nil {
		ctx := context.Background()
		if pol.Cron == "" {
			pol.Cron = st.SettingDefault(ctx, cluster.SettingBackupCron(), "")
		}
		if pol.Store == "" {
			pol.Store = st.SettingDefault(ctx, cluster.SettingBackupStoreOn(), "")
		}
		// Retention belongs with the ship target: both are the answer to "how
		// much history does this cluster pay for". It used to stop at the
		// setting, so backup_retention_days was editable, accepted by the API,
		// and read by nothing — every node pruned at the flag default instead.
		if pol.RetentionDays == 0 {
			pol.RetentionDays = cluster.LoadBackupRetentionDays(ctx, st)
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
	if pol.Store == "" || !isOff(pol.Store) {
		if ak, sk := storeS3Keys(); ak != "" && sk != "" {
			pol.StoreS3 = policyS3{
				Endpoint: storeS3Endpoint(), Bucket: storeS3Bucket(),
				Region: "us-east-1", AccessKey: ak, SecretKey: sk,
			}
		}
	}
	b, err := json.MarshalIndent(pol, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(backupPolicyFile), 0o700); err != nil {
		return err
	}
	tmp := backupPolicyFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, backupPolicyFile)
}

// writeBackupEnv persists the WebDAV target handed to the installer through the
// environment. Refusing to put it on a command line is deliberate: argv is
// world-readable in /proc for the life of the process, and this is a password.
// writeBackupEnv records the in-cluster store as an S3 target in the agent's
// environment.
//
// It used to write a WebDAV URL with the credentials embedded as userinfo. The
// store serves the S3 API on 8333 with the same Store.AccessKey/SecretKey, and
// SeaweedFS creates a bucket on first upload — so the WebDAV gateway, its port and
// its five-minute PUT timeout bought nothing but a second code path.
func writeBackupEnv() error {
	if err := os.MkdirAll(filepath.Dir(backupEnvFile), 0o700); err != nil {
		return err
	}
	pol, _ := readBackupPolicy(backupPolicyFile)
	if pol.Store != "" && isOff(pol.Store) {
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
	temp := backupEnvFile + ".tmp"
	if err := os.WriteFile(temp, []byte(body), 0o600); err != nil {
		return err
	}
	return os.Rename(temp, backupEnvFile)
}

// storeS3Endpoint is the store's S3 API, reachable on the routing mesh.
func storeS3Endpoint() string {
	if v := envOr("PMCLUSTER_BACKUP_STORE_S3_ENDPOINT", ""); v != "" {
		return v
	}
	return "http://127.0.0.1:8333"
}

// storeS3Bucket is where the store keeps artifacts.
func storeS3Bucket() string {
	if v := envOr("PMCLUSTER_BACKUP_STORE_S3_BUCKET", ""); v != "" {
		return v
	}
	return "pmcluster-backups"
}

// storeS3Keys reads the store's own S3 identity from the running service — the
// same two variables SeaweedFS promotes to an admin identity.
func storeS3Keys() (string, string) {
	out, err := exec.Command("docker", "service", "inspect", storeServiceName,
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

// storeServiceName is the Swarm service that carries the in-cluster store.
const storeServiceName = "backup_seaweedfs"

// renderBackupServiceUnit builds the oneshot unit that performs one run.
//
// The run lock lives in the agent, not here: a forced or single-stack run does
// not come through this unit, and the lock has to cover those too. The comment
// that used to justify flock in ExecStart follows, because the reasoning about
// WHERE the lock must sit is the part that was wrong.
//
// The lock is held for the whole run: a second
// trigger (the timer and an on-demand `backup create` landing together) finds
// the lock taken and exits instead of interleaving two freezes on the same
// filesystem. -n makes it fail immediately rather than queue, and -E 0 makes a
// skipped run exit 0 — nothing failed, a run was already in progress.
//
// It must NOT be an ExecStartPre: `flock -n <file>` with no command errors out
// ("bad file descriptor"), and even with a command the lock would be released
// before ExecStart ran, so the unit would not serialise anything at all.
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
`, volumeRoot, archiveDir, stateDir, storage.DefaultVG, storage.DefaultLV, backupEnvFile, exe)
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
// isStorageNode reports whether this host is meant to hold stack data.
//
// It is a question about the node's ROLE, not about its disk. Checking for a local
// LV is not enough: a worker joined with storage provisioned has one, and would
// then be handed an hourly root job that snapshots a volume holding nothing but a
// test stack — observed on production, where the worker also carried a 41 GB backup
// store on a 58 GB disk.
//
// A storage node is a manager (the fleet keeps stack data on managers) or a worker
// that was explicitly marked as one at join time, which sets PMCLUSTER_STORAGE_NODE
// in this unit's environment.
func isStorageNode() (bool, string) {
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

// storageNodeFn is isStorageNode, overridable for tests.
var storageNodeFn = isStorageNode

// removeBackupAgent takes the agent back off a node that should not have one. It
// is idempotent: a node that never had the units just gets two failed removes.
func removeBackupAgent(out io.Writer) {
	if hostOS == "linux" && hasSystemctl() {
		_ = systemctlFn("disable", "--now", "pmcluster-backup.timer")
	}
	removed := false
	for _, p := range []string{backupTimerPath, backupUnitPath} {
		if err := os.Remove(p); err == nil {
			removed = true
		}
	}
	if removed {
		fmt.Fprintf(out, "  → removed %s and %s: this node is not a storage node\n",
			filepath.Base(backupUnitPath), filepath.Base(backupTimerPath))
		if hostOS == "linux" && hasSystemctl() {
			_ = systemctlFn("daemon-reload")
		}
	}
}

func ensureBackupAgentInstalled(out io.Writer) error {
	// No volume, no backup. A worker does not hold stack data, so an agent here
	// would snapshot an empty directory forever.
	if ok, why := storageNodeFn(); !ok {
		fmt.Fprintf(out, "  (not a storage node — %s; the backup agent was not installed)\n", why)
		removeBackupAgent(out)
		return nil
	}
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
	exe = canonicalAgentBinary(exe, os.Stat)
	if err := os.MkdirAll(backupAgentStateDir, 0o755); err != nil {
		// /var/lib needs root; the run itself will fail loudly if it cannot
		// write, so a permission problem here is worth reporting but not fatal
		// to the install.
		fmt.Fprintf(out, "  (could not create %s: %v)\n", backupAgentStateDir, err)
	}
	if err := writeBackupPolicyFile(); err != nil {
		fmt.Fprintf(out, "  (could not write %s: %v — the agent will use this node's stored settings)\n", backupPolicyFile, err)
	}
	if err := writeBackupEnv(); err != nil {
		fmt.Fprintf(out, "  (could not write %s: %v — artifacts will stay local)\n", backupEnvFile, err)
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
