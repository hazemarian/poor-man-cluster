package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
)

func TestPolicyFileDrivesTheDecision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.conf")
	body := `{"enabled":"true","cron":"0 2 * * *","store":"false",
	          "s3":{"endpoint":"https://s3.example","bucket":"b","region":"r","access_key":"AK","secret_key":"SK"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	pol, ok := readBackupPolicy(path)
	if !ok {
		t.Fatal("policy file not read")
	}
	at := func(h, m int) time.Time { return time.Date(2026, 11, 4, h, m, 0, 0, time.Local) }

	if d := decisionFromPolicy(pol, at(2, 0)); !d.Due || !d.Enabled || d.Store || !d.Offsite {
		t.Fatalf("02:00 decision %+v", d)
	}
	if d := decisionFromPolicy(pol, at(3, 0)); d.Due {
		t.Fatalf("03:00 must not be due: %+v", d)
	}
}

func TestDisabledPolicyNeverRuns(t *testing.T) {
	pol := backupPolicy{Enabled: "false", Cron: "* * * * *"}
	d := decisionFromPolicy(pol, time.Now())
	if d.Enabled || d.Due {
		t.Fatalf("disabled policy still due: %+v", d)
	}
}

// The retention window has to reach the node, and the policy file is the only
// channel a root agent has. backup_retention_days was accepted by the API, asked
// for by the setup wizard and read by nothing: every node pruned at the flag
// default of 15 days while every surface said three.
func TestPolicyFileCarriesTheRetentionWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.conf")
	body := `{"enabled":"true","cron":"0 * * * *","retention_days":7}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	pol, ok := readBackupPolicy(path)
	if !ok {
		t.Fatal("policy file not read")
	}
	policy, said := retentionFromPolicy(pol)
	if !said {
		t.Fatal("the policy said retention_days=7 and was ignored")
	}
	if policy.KeepDays != 7 {
		t.Fatalf("KeepDays = %d, want 7", policy.KeepDays)
	}
	// The dense window is not a policy decision: 48 hours of hourly artifacts is
	// what a restore reaches for, whatever the sparse tail is set to.
	if policy.KeepHours != backups.DefaultRetention().KeepHours {
		t.Fatalf("KeepHours = %d, want the %d default", policy.KeepHours, backups.DefaultRetention().KeepHours)
	}
}

// A policy file written before the setting existed says nothing about retention.
// That is not the same statement as "keep for zero days", and it must not be
// read as one — the agent keeps looking (the setting, then the default).
func TestPolicyWithoutRetentionIsUnknownNotZero(t *testing.T) {
	for name, pol := range map[string]backupPolicy{
		"field absent":   {Enabled: "true", Cron: "0 * * * *"},
		"field zero":     {Enabled: "true", Cron: "0 * * * *", RetentionDays: 0},
		"field negative": {Enabled: "true", Cron: "0 * * * *", RetentionDays: -4},
	} {
		t.Run(name, func(t *testing.T) {
			policy, said := retentionFromPolicy(pol)
			if said {
				t.Fatalf("retention %+v was treated as a decision", pol)
			}
			if want := backups.DefaultRetention(); policy != want {
				t.Fatalf("policy = %+v, want the default %+v", policy, want)
			}
		})
	}
}

// The default the fallback uses and the default the CLI documents have to be the
// same number, or the help text describes a retention nobody gets.
func TestDefaultRetentionIsThreeDays(t *testing.T) {
	if got := backups.DefaultRetention().KeepDays; got != 3 {
		t.Fatalf("DefaultRetention().KeepDays = %d, want 3", got)
	}
}

func TestMissingPolicyFileIsNotAnError(t *testing.T) {
	if _, ok := readBackupPolicy(filepath.Join(t.TempDir(), "nope.conf")); ok {
		t.Fatal("a missing file must report absent, not empty")
	}
}
