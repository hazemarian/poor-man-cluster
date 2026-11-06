package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
)

// backupPolicyFile is the node's local copy of the backup settings.
//
// There is no endpoint. The agent runs as root on the host, and the control plane
// reaches it the only way it can reach any node without opening a port: by
// writing a file during install. That file is the schedule, the master switch and
// the ship target; `pmcluster backup-agent install` rewrites it on every run of
// the installer, so refreshing a node is one command rather than an edit.
//
// When the file is absent the agent falls back to this node's copy of the
// control-plane store (managers hold a live one), and then to the defaults — a
// cluster that has not configured backups still gets backups.
const backupPolicyFile = "/etc/pmcluster/backup.conf"

// backupPolicy mirrors the JSON written at install time.
type backupPolicy struct {
	Enabled string   `json:"enabled"`
	Cron    string   `json:"cron"`
	Store   string   `json:"store"`
	S3      policyS3 `json:"s3"`
	// RetentionDays is the sparse window: beyond the dense 48 hours, one
	// artifact per calendar day is kept until this age. It mirrors the
	// backup_retention_days setting, because the agent has no endpoint to ask
	// and the promise "configurable from the console" is only true if the
	// number the operator typed reaches the node. Zero means "unset", and the
	// agent then falls back to the setting (on a node with a store) and to
	// backups.DefaultRetention.
	RetentionDays int `json:"retention_days,omitempty"`
	// KeepLocal says how much history this node keeps on its own disk:
	// "dense" (default, the full retention window) or "minimal" (one artifact
	// per stack — the newest, after every destination confirmed it). The
	// copies that matter are in the store and the bucket; the node's copy is
	// the one that costs disk on the machine running the apps.
	KeepLocal string `json:"keep_local,omitempty"`
	// StoreS3 is the in-cluster store as an S3 target. It used to be a WebDAV URL
	// with the credentials embedded in it; now it is the same five fields as any
	// other destination, so one code path ships to both.
	StoreS3 policyS3 `json:"store_s3,omitempty"`
}

// policyS3 is one destination.
type policyS3 struct {
	Endpoint  string `json:"endpoint"`
	Bucket    string `json:"bucket"`
	Region    string `json:"region"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// config converts the policy entry into the S3 client's shape.
func (p policyS3) config() backups.S3Config {
	return backups.S3Config{
		Endpoint: p.Endpoint, Bucket: p.Bucket, Region: p.Region,
		AccessKey: p.AccessKey, SecretKey: p.SecretKey,
	}
}

// readBackupPolicy loads the node's policy file, if there is one.
func readBackupPolicy(path string) (backupPolicy, bool) {
	var pol backupPolicy
	b, err := os.ReadFile(path)
	if err != nil {
		return pol, false
	}
	if json.Unmarshal(b, &pol) != nil {
		return pol, false
	}
	return pol, true
}

// decisionFromPolicy turns the file into the same decision type the settings
// path produces, so the gate has one shape to reason about.
func decisionFromPolicy(pol backupPolicy, now time.Time) cluster.BackupDecision {
	d := cluster.BackupDecision{
		Enabled:  !isOff(pol.Enabled),
		Schedule: strings.TrimSpace(pol.Cron),
		Store:    !isOff(pol.Store),
	}
	if d.Schedule == "" {
		d.Schedule = cluster.DefaultBackupCron
	}
	d.Offsite = pol.S3.Endpoint != "" && pol.S3.Bucket != "" && pol.S3.AccessKey != "" && pol.S3.SecretKey != ""
	if !d.Enabled {
		d.Reason = "backup_enabled is false in " + backupPolicyFile
		return d
	}
	due, err := cluster.CronDue(d.Schedule, now)
	if err != nil {
		d.Reason = err.Error()
		return d
	}
	d.Due = due
	if !due {
		d.Reason = "schedule " + d.Schedule + " does not match " + now.Format("15:04")
	}
	return d
}

// localRetentionPolicy answers "how long does this node keep artifacts?".
//
// The chain is the one the schedule and the ship targets already follow, and for
// the same reason: the agent runs as root with no endpoint, so a cluster setting
// reaches it through the policy file the installer wrote, and a node that never
// ran the installer still has its own copy of the store. Order is policy file,
// setting, default — the file is the installer's snapshot of the setting and so
// wins; the store is the live truth when the file is absent or predates it.
//
// KeepHours is deliberately not configurable: 48 dense hours is what "restore
// the state from before this morning's deploy" needs, and every artifact inside
// it is the cheap kind of history to keep. Only the sparse tail is a policy
// decision, which is exactly what backup_retention_days names.
func localRetentionPolicy(ctx context.Context) backups.Retention {
	if ctx == nil {
		ctx = context.Background()
	}
	if pol, ok := readBackupPolicy(backupPolicyFile); ok {
		if policy, said := retentionFromPolicy(pol); said {
			return policy
		}
	}
	policy := backups.DefaultRetention()
	if st, _, err := openStore(); err == nil {
		if days := cluster.LoadBackupRetentionDays(ctx, st); days > 0 {
			policy.KeepDays = days
		}
		_ = st.Close()
	}
	return policy
}

// retentionFromPolicy maps a node policy onto the retention policy, and reports
// whether the policy said anything about retention at all.
//
// A policy file written before backup_retention_days reached the installer has
// no retention_days field, and that is not the same statement as "keep nothing":
// it means "unknown", so the caller keeps looking.
func retentionFromPolicy(pol backupPolicy) (backups.Retention, bool) {
	policy := backups.DefaultRetention()
	if pol.RetentionDays <= 0 {
		return policy, false
	}
	policy.KeepDays = pol.RetentionDays
	return policy, true
}

func isOff(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "0", "no", "off":
		return true
	}
	return false
}

// keepLocalMode answers how much history this node keeps on its own disk:
// "dense" (the whole window, the default), "minimal" (one artifact per stack), or
// "none" (nothing — the store and the bucket are the copies). The last value is
// spelled "none"/"off"/"false"/"0".
func keepLocalMode() string {
	raw := ""
	if pol, ok := readBackupPolicy(backupPolicyFile); ok && pol.KeepLocal != "" {
		raw = pol.KeepLocal
	} else {
		raw = os.Getenv("PMCLUSTER_BACKUP_KEEP_LOCAL")
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "none", "off", "false", "0", "no":
		return "none"
	case "minimal", "one", "1":
		return "minimal"
	default:
		return "dense"
	}
}
