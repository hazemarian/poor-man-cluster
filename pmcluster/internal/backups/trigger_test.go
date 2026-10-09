package backups

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseArchivePaths(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "single archive on one line",
			in:   "Successfully created backup at /archive/backup-node1-2026-05-10.tar.gz",
			want: []string{"/archive/backup-node1-2026-05-10.tar.gz"},
		},
		{
			name: "multiple archives across lines",
			in:   "Took /archive/a.tar.gz.\nThen /archive/b.tar.gz now done",
			want: []string{"/archive/a.tar.gz", "/archive/b.tar.gz"},
		},
		{
			name: "no archives",
			in:   "Backup pending; nothing yet.",
			want: nil,
		},
		{
			name: "ignores non-archive paths",
			in:   "/var/log/x.log /archive/c.tar.gz /tmp/y.tar.gz",
			want: []string{"/archive/c.tar.gz"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseArchivePaths(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseArchivePaths(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestTriggerExec_RetriesOnceOnTransientFailure verifies a first-attempt
// failure (the offen agent mid-restart) is retried once and succeeds.
func TestTriggerExec_RetriesOnceOnTransientFailure(t *testing.T) {
	origExec := runBackupExec
	origDelay := backupRetryDelay
	t.Cleanup(func() { runBackupExec = origExec; backupRetryDelay = origDelay })
	backupRetryDelay = time.Millisecond

	calls := 0
	runBackupExec = func(_ context.Context, _ string) (string, string, error) {
		calls++
		if calls == 1 {
			return "", "agent restarting", errors.New("exit status 1")
		}
		return "created backup at /archive/x.tar.gz", "", nil
	}

	res, err := triggerExec(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("triggerExec: %v", err)
	}
	if calls != 2 {
		t.Fatalf("runBackupExec calls = %d, want 2 (one retry)", calls)
	}
	if len(res.ArchivePaths) != 1 || res.ArchivePaths[0] != "/archive/x.tar.gz" {
		t.Errorf("ArchivePaths = %v, want [/archive/x.tar.gz]", res.ArchivePaths)
	}
}

// TestTriggerExec_FailsAfterBothAttempts verifies a persistent failure keeps
// the final (second) error clear instead of silently succeeding.
func TestTriggerExec_FailsAfterBothAttempts(t *testing.T) {
	origExec := runBackupExec
	origDelay := backupRetryDelay
	t.Cleanup(func() { runBackupExec = origExec; backupRetryDelay = origDelay })
	backupRetryDelay = time.Millisecond

	calls := 0
	runBackupExec = func(_ context.Context, _ string) (string, string, error) {
		calls++
		return "", "agent down", errors.New("exit status 1")
	}

	res, err := triggerExec(context.Background(), "abc123")
	if err == nil {
		t.Fatal("triggerExec succeeded, want error")
	}
	if calls != 2 {
		t.Errorf("runBackupExec calls = %d, want 2", calls)
	}
	if !strings.Contains(err.Error(), "docker exec abc123 backup") {
		t.Errorf("error = %q, want docker exec context", err)
	}
	if !strings.Contains(err.Error(), "agent down") {
		t.Errorf("error = %q, want stderr content", err)
	}
	if res == nil || res.Stderr != "agent down" {
		t.Errorf("result stderr = %+v, want 'agent down'", res)
	}
}
