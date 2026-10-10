package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func newSetupTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// assertBackupSettings verifies the backup_s3_* and backup_store_on settings
// persisted by the setup wizard match the expected answers.
func assertBackupSettings(t *testing.T, ctx context.Context, st *store.Store, a setupAnswers) {
	t.Helper()
	want := map[string]string{
		cluster.SettingBackupS3Endpoint():  a.BackupS3Endpoint,
		cluster.SettingBackupS3Bucket():    a.BackupS3Bucket,
		cluster.SettingBackupS3AccessKey(): a.BackupS3AccessKey,
		cluster.SettingBackupS3SecretKey(): a.BackupS3SecretKey,
		cluster.SettingBackupS3Region():    a.BackupS3Region,
		cluster.SettingBackupStoreOn():     a.BackupStoreOn,
	}
	for key, wantVal := range want {
		if got := st.SettingDefault(ctx, key, "UNSET"); got != wantVal {
			t.Errorf("%s = %q, want %q", key, got, wantVal)
		}
	}
}

func TestPersistSetupSecretsOnly_PersistsBackupSettings(t *testing.T) {
	st := newSetupTestStore(t)
	ctx := context.Background()

	a := setupAnswers{
		BackupS3Endpoint:  "https://acct.r2.cloudflarestorage.com",
		BackupS3Bucket:    "offsite-bucket",
		BackupS3AccessKey: "ak",
		BackupS3SecretKey: "sk",
		BackupS3Region:    "auto",
		BackupStoreOn:     "leader",
	}
	if err := persistSetupSecretsOnly(ctx, st, a); err != nil {
		t.Fatalf("persistSetupSecretsOnly: %v", err)
	}
	assertBackupSettings(t, ctx, st, a)
}

func TestPersistSetupSecretsOnly_EmptyS3LeavesThemEmpty(t *testing.T) {
	st := newSetupTestStore(t)
	ctx := context.Background()

	// No offsite S3: the backup_s3_* settings must be persisted empty and the
	// store placement defaults to worker (empty).
	a := setupAnswers{BackupStoreOn: "worker"}
	if err := persistSetupSecretsOnly(ctx, st, a); err != nil {
		t.Fatalf("persistSetupSecretsOnly: %v", err)
	}
	assertBackupSettings(t, ctx, st, a)
	if got := st.SettingDefault(ctx, cluster.SettingBackupS3Endpoint(), ""); got != "" {
		t.Errorf("backup_s3_endpoint = %q, want empty when offsite S3 is not configured", got)
	}
}

func TestPersistSetup_PersistsBackupSettings(t *testing.T) {
	st := newSetupTestStore(t)
	ctx := context.Background()

	a := setupAnswers{
		BackupS3Endpoint:  "https://s3.example.com",
		BackupS3Bucket:    "bucket",
		BackupS3AccessKey: "access",
		BackupS3SecretKey: "secret",
		BackupS3Region:    "us-east-1",
		BackupStoreOn:     "worker",
	}
	if err := persistSetup(ctx, st, a); err != nil {
		t.Fatalf("persistSetup: %v", err)
	}
	assertBackupSettings(t, ctx, st, a)
}

// TestStorageFailoverEnabled mirrors the wizard's gate: automatic failover
// must only enable when an offsite S3 destination is configured (the
// recoverable copy lives in S3).
func TestStorageFailoverEnabled(t *testing.T) {
	cases := []struct {
		name       string
		requested  bool
		s3Endpoint string
		s3Bucket   string
		want       bool
	}{
		{"requested with full S3", true, "https://s3.example.com", "bucket", true},
		{"not requested with full S3", false, "https://s3.example.com", "bucket", false},
		{"requested without endpoint", true, "", "bucket", false},
		{"requested without bucket", true, "https://s3.example.com", "", false},
		{"requested without any S3", true, "", "", false},
	}
	for _, c := range cases {
		if got := storageFailoverEnabled(c.requested, c.s3Endpoint, c.s3Bucket); got != c.want {
			t.Errorf("%s: storageFailoverEnabled(%v, %q, %q) = %v, want %v",
				c.name, c.requested, c.s3Endpoint, c.s3Bucket, got, c.want)
		}
	}
}
