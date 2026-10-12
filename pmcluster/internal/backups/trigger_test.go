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
	stubExec(t)
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

// TestTriggerExec_FailsAfterExhaustingAttempts verifies a persistent failure is
// retried to the bound and then reported, keeping the last error clear instead
// of silently succeeding.
func TestTriggerExec_FailsAfterExhaustingAttempts(t *testing.T) {
	stubExec(t)
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
	if calls != backupMaxAttempts {
		t.Errorf("runBackupExec calls = %d, want %d", calls, backupMaxAttempts)
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

// The failing exec's stderr is USUALLY EMPTY: offen writes to its own container
// streams, which `docker exec` does not capture. That is why the message used to
// be "exit status 1 ()" — an exit code and no cause. The agent's log has to be
// attached instead.
func TestTriggerExec_ReportsTheAgentsOwnLogWhenTheExecIsSilent(t *testing.T) {
	stubExec(t)
	backupRetryDelay = time.Millisecond
	runBackupExec = func(_ context.Context, _ string) (string, string, error) {
		return "", "", errors.New("exit status 1") // silent, as offen is mid-startup
	}
	agentLogTail = func(context.Context, string, int) string { return "level=ERROR msg=\"store unreachable\"" }

	_, err := triggerExec(context.Background(), "abc123")
	if err == nil {
		t.Fatal("triggerExec succeeded, want error")
	}
	if !strings.Contains(err.Error(), "store unreachable") {
		t.Errorf("error does not carry the agent's own log, so it still gives no cause: %q", err)
	}
}

// When the agent has logged nothing either, say what to check rather than
// printing an empty pair of brackets.
func TestTriggerExec_SaysWhatToCheckWhenEvenTheAgentIsSilent(t *testing.T) {
	stubExec(t)
	backupRetryDelay = time.Millisecond
	runBackupExec = func(_ context.Context, _ string) (string, string, error) {
		return "", "", errors.New("exit status 1")
	}
	agentLogTail = func(context.Context, string, int) string { return "" }

	_, err := triggerExec(context.Background(), "abc123")
	if err == nil {
		t.Fatal("triggerExec succeeded, want error")
	}
	if !strings.Contains(err.Error(), "still starting") || !strings.Contains(err.Error(), "backup_volume-backup") {
		t.Errorf("error should say the agent may still be starting and how to check: %q", err)
	}
	if strings.Contains(err.Error(), "()") {
		t.Errorf("error still renders an empty cause: %q", err)
	}
}

// The retry is bounded by the caller's deadline, not only by the attempt count,
// so `backup create --timeout=20s` does not outlive its own flag.
func TestTriggerExec_StopsRetryingAtTheDeadline(t *testing.T) {
	stubExec(t)
	backupRetryDelay = time.Hour // would hang if the deadline were not honoured

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	runBackupExec = func(_ context.Context, _ string) (string, string, error) {
		calls++
		cancel() // the caller gives up during the first attempt
		return "", "", errors.New("exit status 1")
	}

	if _, err := triggerExec(ctx, "abc123"); err == nil {
		t.Fatal("triggerExec succeeded, want error")
	}
	if calls != 1 {
		t.Errorf("runBackupExec calls = %d, want 1 — a cancelled context must not be retried", calls)
	}
}

// stubExec saves and restores the package seams the trigger tests replace.
func stubExec(t *testing.T) {
	t.Helper()
	origExec, origDelay, origLog := runBackupExec, backupRetryDelay, agentLogTail
	t.Cleanup(func() { runBackupExec, backupRetryDelay, agentLogTail = origExec, origDelay, origLog })
}
