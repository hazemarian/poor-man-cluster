package doctor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// doctor must be honest about what it could not measure: a host without a
// swarm, a store or a backup state dir reports skipped checks with reasons, and
// "nothing failed" must not mean "nothing was checked".
func TestRunSkipsWhatItCannotMeasure(t *testing.T) {
	rep, err := Run(context.Background(), Input{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Checks) == 0 {
		t.Fatal("no checks reported")
	}
	for _, c := range rep.Checks {
		if c.Status == "" {
			t.Errorf("check %q has no status", c.Name)
		}
		if c.Status == StatusSkip && c.Detail == "" {
			t.Errorf("check %q is skipped without a reason", c.Name)
		}
	}
	if !rep.OK {
		t.Error("a host with nothing to check must not report failure")
	}
}

// A restore drill that has never run is a warning, not a pass: the whole point
// of the check is that "we have not tried" is not "we are fine".
func TestRestoreDrillIsAWarningWhenNoDrillHasRun(t *testing.T) {
	c := checkDrill(Input{BackupStateDir: t.TempDir()})
	if c.Status != StatusWarn {
		t.Fatalf("status = %q, want warn (%s)", c.Status, c.Detail)
	}
}

// A failed drill is a failure, and the detail names when it failed.
func TestRestoreDrillReportsTheLastFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "drill"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{"last":{"at":"` + time.Now().UTC().Format(time.RFC3339) + `","archive":"a.tar.gz","ok":false,"error":"sqlite locked"}}`
	if err := os.WriteFile(filepath.Join(dir, "drill", "status.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := checkDrill(Input{BackupStateDir: dir}); c.Status != StatusFail {
		t.Fatalf("status = %q, want fail (%s)", c.Status, c.Detail)
	}
}

// The offsite check is the one that catches a bucket that expired its objects,
// so a recorded miss has to fail the doctor — not warn.
func TestOffsiteMissIsAFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "offsite"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{"last":{"at":"` + time.Now().UTC().Format(time.RFC3339) + `","target":"s3:b","checked":10,"present":8,"missing":["a.tar.gz","b.tar.gz"],"ok":false}}`
	if err := os.WriteFile(filepath.Join(dir, "offsite", "status.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c := checkOffsite(Input{BackupStateDir: dir})
	if c.Status != StatusFail {
		t.Fatalf("status = %q, want fail (%s)", c.Status, c.Detail)
	}
}

// A worker cannot read the manager set; it must say so rather than report a
// healthy swarm it never inspected.
func TestSwarmIsSkippedOnAWorker(t *testing.T) {
	c := checkSwarm(context.Background(), Input{Docker: workerInfo{}})
	if c.Status != StatusSkip {
		t.Fatalf("status = %q, want skip (%s)", c.Status, c.Detail)
	}
}

// workerInfo reports a swarm the node does not control.
type workerInfo struct{ runtime.Client }

func (w workerInfo) Info(context.Context) (runtime.Info, error) {
	return runtime.Info{SwarmControlAvailable: false}, nil
}
