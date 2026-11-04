package cli

import (
	"encoding/json"
	"os"
	"strings"
	"time"

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
	Enabled string `json:"enabled"`
	Cron    string `json:"cron"`
	Store   string `json:"store"`
	S3      struct {
		Endpoint  string `json:"endpoint"`
		Bucket    string `json:"bucket"`
		Region    string `json:"region"`
		AccessKey string `json:"access_key"`
		SecretKey string `json:"secret_key"`
	} `json:"s3"`
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

func isOff(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "0", "no", "off":
		return true
	}
	return false
}
