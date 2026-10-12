package cluster

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func preflightStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func setOffsite(t *testing.T, st *store.Store, endpoint string) {
	t.Helper()
	ctx := context.Background()
	for k, v := range map[string]string{
		SettingBackupS3Endpoint():  endpoint,
		SettingBackupS3Bucket():    "pmclusterbackup",
		SettingBackupS3AccessKey(): "AKIA",
		SettingBackupS3SecretKey(): "secret",
		SettingBackupS3Region():    "eu-central-3",
	} {
		if err := st.SetSetting(ctx, k, v); err != nil {
			t.Fatalf("SetSetting %s: %v", k, err)
		}
	}
}

// TestCheckBackupOffsiteRefusesADeletedBucket: applying a backup configuration
// whose bucket does not exist must fail loudly, at configuration time. This is
// the production failure this check exists for: the bucket was deleted, every
// scheduled run failed its offsite copy, and the audit table still reported
// success.
func TestCheckBackupOffsiteRefusesADeletedBucket(t *testing.T) {
	ctx := context.Background()
	st := preflightStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `<?xml version="1.0"?><Error><Code>NoSuchBucket</Code><BucketName>pmclusterbackup</BucketName></Error>`)
	}))
	defer srv.Close()
	setOffsite(t, st, srv.URL)

	var out bytes.Buffer
	err := checkBackupOffsite(ctx, UpdateDeps{Store: st, Stdout: &out})
	if err == nil {
		t.Fatal("a deleted bucket was accepted; the offsite copy would fail on every run")
	}
	for _, want := range []string{"pmclusterbackup", "does not exist"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

// TestCheckBackupOffsiteWarnsButContinuesWhenUnreachable: an endpoint outage
// must not become an accidental prerequisite for bringing a cluster up.
func TestCheckBackupOffsiteWarnsButContinuesWhenUnreachable(t *testing.T) {
	ctx := context.Background()
	st := preflightStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := srv.URL
	srv.Close() // nothing is listening now
	setOffsite(t, st, dead)

	var out bytes.Buffer
	if err := checkBackupOffsite(ctx, UpdateDeps{Store: st, Stdout: &out}); err != nil {
		t.Fatalf("an unreachable endpoint failed the apply: %v", err)
	}
	if !strings.Contains(out.String(), "unreachable") {
		t.Errorf("no warning printed for an unreachable destination; got %q", out.String())
	}
}

// TestCheckBackupOffsiteNoopWithoutSettings: no offsite configured is normal.
func TestCheckBackupOffsiteNoopWithoutSettings(t *testing.T) {
	var out bytes.Buffer
	if err := checkBackupOffsite(context.Background(), UpdateDeps{Store: preflightStore(t), Stdout: &out}); err != nil {
		t.Fatalf("no offsite configured should never fail: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("unexpected output with no offsite configured: %q", out.String())
	}
}
