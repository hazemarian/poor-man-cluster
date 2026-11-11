package cli

import (
	"context"
	"io"
	"path/filepath"
	"strings"
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

// withSetupApp stores a bare App row. The runner gate only asks whether an App
// EXISTS (the key it validates is proven by githubapp.Local.Apply, which needs a
// real PEM and a network), so this is enough to reach the runner checks.
func withSetupApp(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.SetGitHubApp(context.Background(), &store.GitHubApp{
		AppID: 1, InstallationID: 2, PrivateKeyCiphertext: []byte("not-decrypted-here"),
	}); err != nil {
		t.Fatalf("SetGitHubApp: %v", err)
	}
}

// TestRunnerSettingsPersistSocketPathAndInterval: the socket path and the poll
// interval are runner settings a fresh install must be able to state once. They
// are persisted by the same path the other runner answers take (the wizard's
// direct write, not the settings allowlist), so nothing else would catch a
// dropped key.
func TestRunnerSettingsPersistSocketPathAndInterval(t *testing.T) {
	st := newSetupTestStore(t)
	ctx := context.Background()

	a := setupAnswers{
		GitHubRunnerSocketPath:         "/run/pmcluster/docker.sock",
		GitHubRunnerAssignmentInterval: "30",
		GitHubRunnerOrg:                "acme",
		GitHubRunnerNodes:              "worker-1",
		GitHubRunnerGroup:              "pmcluster",
		GitHubRunnerSocketProxy:        true,
	}
	if err := persistSetupSecretsOnly(ctx, st, a); err != nil {
		t.Fatalf("persistSetupSecretsOnly: %v", err)
	}
	if got := st.SettingDefault(ctx, cluster.SettingGitHubRunnerSocketPath(), ""); got != a.GitHubRunnerSocketPath {
		t.Errorf("github_runner_socket_path = %q, want %q", got, a.GitHubRunnerSocketPath)
	}
	if got := st.SettingDefault(ctx, cluster.SettingGitHubRunnerAssignmentInterval(), ""); got != "30" {
		t.Errorf("github_runner_assignment_interval = %q, want 30", got)
	}
}

// TestValidateRunnerSetupRefusesBadSocketPathAndInterval: setup writes settings
// directly, so it cannot rely on the settings surface to reject a relative
// socket path or a poll interval that would hammer the leader.
func TestValidateRunnerSetupRefusesBadSocketPathAndInterval(t *testing.T) {
	cases := []struct {
		name string
		a    setupAnswers
		want string
	}{
		{
			name: "relative socket path",
			a: setupAnswers{
				GitHubRunnersEnabled: true, GitHubRunnerOrg: "acme",
				GitHubRunnerNodes: "worker-1", GitHubRunnerGroup: "pmcluster",
				GitHubRunnerSocketPath: "docker.sock",
			},
			want: "github-runner-socket-path must be an absolute path",
		},
		{
			name: "interval below the floor",
			a: setupAnswers{
				GitHubRunnersEnabled: true, GitHubRunnerOrg: "acme",
				GitHubRunnerNodes: "worker-1", GitHubRunnerGroup: "pmcluster",
				GitHubRunnerAssignmentInterval: "5",
			},
			want: "between 10 and 3600",
		},
		{
			name: "interval that is not a number",
			a: setupAnswers{
				GitHubRunnersEnabled: true, GitHubRunnerOrg: "acme",
				GitHubRunnerNodes: "worker-1", GitHubRunnerGroup: "pmcluster",
				GitHubRunnerAssignmentInterval: "a minute",
			},
			want: "whole number of seconds",
		},
	}
	for _, c := range cases {
		st := newSetupTestStore(t)
		withSetupApp(t, st)
		err := validateRunnerSetup(context.Background(), st, c.a, io.Discard)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: validateRunnerSetup = %v, want an error containing %q", c.name, err, c.want)
		}
	}
}
