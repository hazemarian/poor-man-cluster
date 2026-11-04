package backups

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The agent prints one line per stack; only the archive name matters here.
func TestParseArchivePaths(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "agent log line",
			in:   "✓ horizon-erp   1.6 MB  backup-node1-2026-11-03T23-37-02-stack-horizon-erp.tar.gz  [verified sqlite]",
			want: []string{"backup-node1-2026-11-03T23-37-02-stack-horizon-erp.tar.gz"},
		},
		{
			name: "several stacks",
			in: "✓ a   1 B  backup-n-2026-11-03T00-00-00-stack-a.tar.gz  [verified sqlite]\n" +
				"✓ b   2 B  backup-n-2026-11-03T00-00-00-stack-b.tar.gz  [verified postgres]",
			want: []string{"backup-n-2026-11-03T00-00-00-stack-a.tar.gz", "backup-n-2026-11-03T00-00-00-stack-b.tar.gz"},
		},
		{name: "trailing punctuation", in: "wrote backup-n-1T00-00-00-stack-x.tar.gz.", want: []string{"backup-n-1T00-00-00-stack-x.tar.gz"}},
		{name: "nothing", in: "node n · 0 stack(s) · wrote blocked for 0s", want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseArchivePaths(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// stubTrigger swaps the process seams for one test.
func stubTrigger(t *testing.T, euid int, run func(context.Context, string) (string, string, error)) *int {
	t.Helper()
	calls := 0
	origEUID, origRun, origBin := geteuid, runAgent, agentBinary
	geteuid = func() int { return euid }
	agentBinary = func() string { return "/usr/local/bin/pmcluster" }
	runAgent = func(ctx context.Context, bin string) (string, string, error) {
		calls++
		return run(ctx, bin)
	}
	t.Cleanup(func() { geteuid, runAgent, agentBinary = origEUID, origRun, origBin })
	return &calls
}

func TestTriggerRunsTheAgentAndReturnsItsArtifacts(t *testing.T) {
	calls := stubTrigger(t, 0, func(_ context.Context, bin string) (string, string, error) {
		if bin != "/usr/local/bin/pmcluster" {
			t.Fatalf("ran %q", bin)
		}
		return "✓ app  1.0 MB  backup-n-2026-11-03T00-00-00-stack-app.tar.gz  [verified sqlite]\n" +
			"node n · 1 stack(s) · wrote blocked for 10ms", "", nil
	})
	res, err := Trigger(context.Background())
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("agent invoked %d times, want 1", *calls)
	}
	if len(res.ArchivePaths) != 1 || !strings.Contains(res.ArchivePaths[0], "stack-app.tar.gz") {
		t.Fatalf("paths = %v", res.ArchivePaths)
	}
}

// Freezing a filesystem needs root. Saying so — with the command to run — is
// much better than a permission error from deep inside LVM.
func TestTriggerRefusesWithoutRootAndSaysWhatToRun(t *testing.T) {
	calls := stubTrigger(t, 1000, func(context.Context, string) (string, string, error) {
		return "", "", nil
	})
	_, err := Trigger(context.Background())
	if err == nil {
		t.Fatal("Trigger ran without root")
	}
	for _, want := range []string{"needs root", "sudo pmcluster backup-agent run", "list-timers"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error does not mention %q: %v", want, err)
		}
	}
	if *calls != 0 {
		t.Fatal("invoked the agent despite refusing")
	}
}

// A failing run must carry the reason, not just an exit status: the caller is an
// HTTP handler, and "exit status 1" tells the operator nothing.
func TestTriggerSurfacesTheAgentsReason(t *testing.T) {
	stubTrigger(t, 0, func(context.Context, string) (string, string, error) {
		return "", "verify: /var/stack/data is not mounted — refusing to run", errors.New("exit status 1")
	})
	_, err := Trigger(context.Background())
	if err == nil {
		t.Fatal("a failed run returned no error")
	}
	if !strings.Contains(err.Error(), "not mounted") {
		t.Fatalf("error dropped the agent's reason: %v", err)
	}
}

func TestTriggerFallsBackToStdoutWhenStderrIsEmpty(t *testing.T) {
	stubTrigger(t, 0, func(context.Context, string) (string, string, error) {
		return "pool at 91% — aborting", "", errors.New("exit status 1")
	})
	_, err := Trigger(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pool at 91%") {
		t.Fatalf("error = %v", err)
	}
}

type checkpointRecorder struct {
	called bool
	err    error
}

func (c *checkpointRecorder) WALCheckpoint(context.Context) error {
	c.called = true
	return c.err
}

// A stack move triggers a backup first; the control plane's SQLite WAL must be
// folded into its main file before the snapshot, and a checkpoint failure must
// abort rather than silently produce an archive missing the newest writes.
func TestLocalTriggerCheckpointsBeforeRunning(t *testing.T) {
	rec := &checkpointRecorder{}
	stubTrigger(t, 0, func(context.Context, string) (string, string, error) { return "", "", nil })
	if _, err := (LocalTrigger{Store: rec}).Trigger(context.Background()); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if !rec.called {
		t.Fatal("WAL checkpoint was skipped")
	}

	rec = &checkpointRecorder{err: errors.New("db locked")}
	stubTrigger(t, 0, func(context.Context, string) (string, string, error) {
		t.Fatal("the agent ran despite a failed checkpoint")
		return "", "", nil
	})
	if _, err := (LocalTrigger{Store: rec}).Trigger(context.Background()); err == nil {
		t.Fatal("checkpoint failure did not abort the backup")
	}
}

// The daemon runs as root on platform nodes; the check must use the process's
// real uid rather than a build-time assumption.
func TestGeteuidIsTheProcessEUID(t *testing.T) {
	if got, want := geteuid(), os.Geteuid(); got != want {
		t.Fatalf("geteuid() = %d, want %d", got, want)
	}
}
