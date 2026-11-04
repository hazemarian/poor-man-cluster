package cluster

import (
	"context"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// BackupDecision is the control plane's answer to "should this node back up
// right now?".
//
// The schedule belongs to the cluster's settings, not to a systemd timer. The
// timer is a heartbeat — it fires, asks this question, and does nothing when the
// answer is no. That is the whole point: a schedule is configuration, and
// configuration that lives in a unit file is configuration a user can silently
// break.
type BackupDecision struct {
	// Enabled is the master switch (setting backup_enabled).
	Enabled bool `json:"enabled"`
	// Due is Enabled AND the schedule matching the current minute.
	Due bool `json:"due"`
	// Schedule is the cron expression in force, echoed back for the log.
	Schedule string `json:"schedule"`
	// Store is true when artifacts should also be kept in one place in the
	// cluster (the SeaweedFS store). False means node-local only.
	Store bool `json:"store"`
	// Offsite is true when an S3-compatible destination is fully configured.
	Offsite bool `json:"offsite"`

	// Reason explains a "no", in the words the operator will read.
	Reason string `json:"reason,omitempty"`
}

// DefaultBackupCron is hourly: the storage-failover path restores the newest
// archive, so an hour is the widest useful gap.
const DefaultBackupCron = "0 * * * *"

// BackupDecisionFor computes the decision from the cluster's settings.
//
// Defaults are deliberately permissive for existing clusters: a cluster that has
// been running backups since before the switch existed must not stop backing up
// because the new setting is absent.
func BackupDecisionFor(ctx context.Context, st *store.Store, now time.Time) BackupDecision {
	d := BackupDecision{Schedule: DefaultBackupCron}

	enabled := strings.TrimSpace(st.SettingDefault(ctx, settingBackupEnabled, "true"))
	d.Enabled = enabled != "false" && enabled != "0" && enabled != "no"

	if cron := strings.TrimSpace(st.SettingDefault(ctx, settingBackupCron, "")); cron != "" {
		d.Schedule = cron
	}

	// backup_store_on used to mean which node class hosts the store. "off" is
	// now also meaningful: the operator said backups stay on the node.
	switch strings.ToLower(strings.TrimSpace(st.SettingDefault(ctx, settingBackupStoreOn, ""))) {
	case "off", "none", "false", "0":
		d.Store = false
	default:
		d.Store = true
	}

	// Credentials are NOT part of this answer. The endpoint is reachable from the
	// whole private plane, so anything in the body is readable by every container
	// on the overlay — a bucket key in a response would hand an attacker the
	// ability to delete the backups. The agent reads its destination from its own
	// 0600 env file instead.
	d.Offsite = BackupS3For(ctx, st).Configured()

	switch {
	case !d.Enabled:
		d.Reason = "backup_enabled is false"
	default:
		due, err := CronDue(d.Schedule, now)
		if err != nil {
			// A bad expression must never be read as "due": it would turn a
			// typo into an hourly run at the operator's expense.
			d.Reason = err.Error()
			return d
		}
		d.Due = due
		if !due {
			d.Reason = "schedule " + d.Schedule + " does not match " + now.Format("15:04")
		}
	}
	return d
}

// backupEnabled reads the master switch for the rendered policy. Absent means
// true: a cluster that has been backing up since before the switch existed must
// not stop because the settings row is missing.
func backupEnabled(ctx context.Context, st *store.Store) bool {
	v := strings.ToLower(strings.TrimSpace(st.SettingDefault(ctx, settingBackupEnabled, "true")))
	return v != "false" && v != "0" && v != "no"
}

// backupCron reads the schedule for the rendered policy.
func backupCron(ctx context.Context, st *store.Store) string {
	if v := strings.TrimSpace(st.SettingDefault(ctx, settingBackupCron, "")); v != "" {
		return v
	}
	return DefaultBackupCron
}

// BackupS3For reads the offsite destination from the cluster settings.
func BackupS3For(ctx context.Context, st *store.Store) (cfg S3Settings) {
	cfg = S3Settings{
		Endpoint:  st.SettingDefault(ctx, settingBackupS3Endpoint, ""),
		Bucket:    st.SettingDefault(ctx, settingBackupS3Bucket, ""),
		AccessKey: st.SettingDefault(ctx, settingBackupS3AccessKey, ""),
		SecretKey: st.SettingDefault(ctx, settingBackupS3SecretKey, ""),
		Region:    st.SettingDefault(ctx, settingBackupS3Region, ""),
	}
	return cfg
}

// S3Settings is the offsite destination as stored in the cluster settings.
// It is deliberately a plain struct with no methods that log or print, because
// the secret must never reach a log line.
type S3Settings struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
}

// Configured reports whether every field an upload needs is present.
func (s S3Settings) Configured() bool {
	return s.Endpoint != "" && s.Bucket != "" && s.AccessKey != "" && s.SecretKey != ""
}
