package doctor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runners"
)

// writeRecord puts a runner agent record on disk, exactly as the daemon writes
// it (0600, JSON), so the check reads the same file it reads in production.
func writeRecord(t *testing.T, rec runners.AgentRecord) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "runner.json")
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCheckLocalRunnerMemory is the check that would have caught the incident on
// the node itself: a 2560m cap on a 1839 MB machine. A worker has no store, so
// the cap comes from the assignment it applied and the size from its own RAM.
func TestCheckLocalRunnerMemory(t *testing.T) {
	t.Run("cap larger than the node fails, naming both numbers", func(t *testing.T) {
		path := writeRecord(t, runners.AgentRecord{Node: "nxt-sw-3-w", Memory: "2560m", Enabled: true})
		c := checkLocalRunnerMemory(NodeInput{Hostname: "nxt-sw-3-w", StatePath: path, MemTotalMiB: 1839})
		if c.Status != StatusFail {
			t.Fatalf("status = %q (%s), want fail", c.Status, c.Detail)
		}
		for _, want := range []string{"1839", "2560m"} {
			if !strings.Contains(c.Detail, want) {
				t.Errorf("detail is missing %q: %s", want, c.Detail)
			}
		}
	})

	t.Run("the same cap on a bigger node passes", func(t *testing.T) {
		path := writeRecord(t, runners.AgentRecord{Node: "nxt-sw-6-w", Memory: "2560m", Enabled: true})
		c := checkLocalRunnerMemory(NodeInput{Hostname: "nxt-sw-6-w", StatePath: path, MemTotalMiB: 3852})
		if c.Status != StatusOK {
			t.Fatalf("status = %q (%s), want ok", c.Status, c.Detail)
		}
		if !strings.Contains(c.Detail, "70%") {
			t.Errorf("detail does not state the ceiling: %s", c.Detail)
		}
	})

	t.Run("unknown RAM is a skip, never a guess", func(t *testing.T) {
		path := writeRecord(t, runners.AgentRecord{Node: "w", Memory: "2560m"})
		c := checkLocalRunnerMemory(NodeInput{Hostname: "w", StatePath: path, MemTotalMiB: 0})
		if c.Status != StatusSkip {
			t.Fatalf("status = %q (%s), want skip", c.Status, c.Detail)
		}
	})

	t.Run("no assignment applied yet is a skip", func(t *testing.T) {
		c := checkLocalRunnerMemory(NodeInput{Hostname: "w", StatePath: filepath.Join(t.TempDir(), "absent.json"), MemTotalMiB: 1839})
		if c.Status != StatusSkip {
			t.Fatalf("status = %q (%s), want skip", c.Status, c.Detail)
		}
	})
}

// TestRunNodeLocal_IncludesTheMemoryCheck keeps the check wired into the report
// an operator actually runs on a worker.
func TestRunNodeLocal_IncludesTheMemoryCheck(t *testing.T) {
	path := writeRecord(t, runners.AgentRecord{Node: "nxt-sw-3-w", Memory: "2560m", Enabled: true})
	rep := RunNodeLocal(context.Background(), NodeInput{
		Hostname: "nxt-sw-3-w", StatePath: path, MemTotalMiB: 1839,
		Health: func(context.Context, string) (int, error) { return 200, nil },
	})
	var found *Check
	for i := range rep.Checks {
		if rep.Checks[i].Name == "runner memory" {
			found = &rep.Checks[i]
		}
	}
	if found == nil {
		t.Fatalf("the report has no \"runner memory\" check: %+v", rep.Checks)
	}
	if found.Status != StatusFail {
		t.Errorf("status = %q, want fail (this is the node that was OOM-killed)", found.Status)
	}
	if rep.OK {
		t.Error("the report is OK with a failing check in it")
	}
}
