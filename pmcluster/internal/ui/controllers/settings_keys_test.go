package controllers

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	pmsettings "github.com/hazemarian/poor-man-cluster/pmcluster/internal/settings"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// The form may not post a setting the daemon rejects, and must carry the backup
// settings it is responsible for.
//
// The comment on clusterSettingKeys says the two lists must mirror exactly, and
// nothing enforced it: backup_retention_days was accepted by the API and absent
// from the form, so the only way to change how long a cluster keeps its backups
// was a setting no operator could see. One-directional here on purpose — a
// surplus key fails every save, while a key the daemon has and the form does not
// is a deliberate choice (the S3 credentials are not edited from this page).
func TestConsoleSettingsMirrorTheDaemonAllowlist(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	daemon, err := pmsettings.NewLocal(st).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	console := map[string]bool{}
	for _, k := range clusterSettingKeys {
		console[k] = true
	}

	// Not every daemon setting belongs on this form: the offsite S3 credentials
	// and the observability retentions are deliberately not editable here yet.
	// What must hold is the backup triad the form IS responsible for.
	for _, k := range []string{"backup_enabled", "backup_cron", "backup_retention_days"} {
		if !console[k] {
			t.Fatalf("the console form omits %s, which it is responsible for", k)
		}
	}

	var unknown []string
	for _, k := range clusterSettingKeys {
		if _, ok := daemon[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Fatalf("the console form posts settings the daemon rejects (the save would fail): %v", unknown)
	}
}

// The retention field is the one numeric setting on the form. It decides how
// much history survives a prune, so a typo must be refused at the form rather
// than stored: the daemon stores any string, and LoadBackupRetentionDays then
// quietly falls back to the default — the operator sees a saved value that the
// agent ignores.
func TestRetentionFieldIsOnTheForm(t *testing.T) {
	var found bool
	for _, k := range clusterSettingKeys {
		if k == "backup_retention_days" {
			found = true
		}
	}
	if !found {
		t.Fatal("backup_retention_days is not editable from the console")
	}
	if clusterBoolSettings["backup_retention_days"] {
		t.Fatal("backup_retention_days is a number, not a flag — a checkbox would store true/false")
	}
}
