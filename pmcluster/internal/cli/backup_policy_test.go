package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestMissingPolicyFileIsNotAnError(t *testing.T) {
	if _, ok := readBackupPolicy(filepath.Join(t.TempDir(), "nope.conf")); ok {
		t.Fatal("a missing file must report absent, not empty")
	}
}
