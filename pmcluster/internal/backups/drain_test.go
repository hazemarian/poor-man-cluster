package backups

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// fakeSwarm is a Scaler backed by an in-memory service list.
type fakeSwarm struct {
	mu       sync.Mutex
	services map[string]runtime.Service // name -> service
	scaleLog []string
	// refuseZero makes ScaleService(0) fail, to exercise the rollback path.
	refuseZero bool
	// stick makes tasks keep running whatever the desired count says, to
	// exercise the "still running" timeout.
	stick bool
}

func newFakeSwarm(svcs ...runtime.Service) *fakeSwarm {
	m := map[string]runtime.Service{}
	for _, s := range svcs {
		m[s.Name] = s
	}
	return &fakeSwarm{services: m}
}

func (f *fakeSwarm) ServiceList(context.Context) ([]runtime.Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]runtime.Service, 0, len(f.services))
	for _, s := range f.services {
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeSwarm) ScaleService(_ context.Context, serviceID string, replicas uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, s := range f.services {
		if s.ID != serviceID {
			continue
		}
		if replicas == 0 && f.refuseZero {
			return errBoom
		}
		f.scaleLog = append(f.scaleLog, name+"="+itoa(replicas))
		s.Desired = replicas
		if !f.stick {
			s.Replicas = replicas
		}
		f.services[name] = s
		return nil
	}
	return nil
}

func (f *fakeSwarm) replicasOf(name string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.services[name].Desired
}

type boomErr struct{}

func (boomErr) Error() string { return "boom" }

var errBoom = boomErr{}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func shopStack() *fakeSwarm {
	return newFakeSwarm(
		runtime.Service{ID: "s1", Name: "shop_app", Stack: "shop", Mode: "replicated", Desired: 2, Replicas: 2},
		runtime.Service{ID: "s2", Name: "shop_db", Stack: "shop", Mode: "replicated", Desired: 1, Replicas: 1},
		runtime.Service{ID: "s3", Name: "shop_agent", Stack: "shop", Mode: "global", Replicas: 3},
		runtime.Service{ID: "o1", Name: "other_app", Stack: "other", Mode: "replicated", Desired: 4, Replicas: 4},
	)
}

// A restore writes into the live volume and then unlinks what the snapshot did
// not hold. Draining is the difference between that and a database built out of
// two different moments.
func TestDrainPausesTheStacksServicesAndPutsThemBack(t *testing.T) {
	sw := shopStack()
	d := &Drainer{Scaler: sw, Wait: 2 * time.Second}

	rep, undo, err := d.Drain(context.Background(), "shop")
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got := rep.Paused["shop_app"]; got != 2 {
		t.Errorf("recorded replicas for shop_app = %d, want 2 (what it had before)", got)
	}
	if len(rep.Paused) != 2 {
		t.Errorf("paused %v, want the two replicated services", rep.Names())
	}
	if strings.Join(rep.Skipped, ",") != "shop_agent" {
		t.Errorf("skipped = %v, want the global service named so the operator is told", rep.Skipped)
	}
	if sw.replicasOf("shop_app") != 0 || sw.replicasOf("shop_db") != 0 {
		t.Error("services still desired after drain")
	}
	if sw.replicasOf("other_app") != 4 {
		t.Error("drain touched a service belonging to another stack")
	}

	undo()
	if sw.replicasOf("shop_app") != 2 || sw.replicasOf("shop_db") != 1 {
		t.Errorf("after undo: shop_app=%d shop_db=%d, want 2 and 1",
			sw.replicasOf("shop_app"), sw.replicasOf("shop_db"))
	}
}

// A drain that cannot stop the workload must fail, not proceed quietly: the
// whole point is that the restore does not run under a live database.
func TestDrainRefusesWhenTasksDoNotStop(t *testing.T) {
	sw := shopStack()
	sw.stick = true
	d := &Drainer{Scaler: sw, Wait: 40 * time.Millisecond}

	_, _, err := d.Drain(context.Background(), "shop")
	if err == nil {
		t.Fatal("Drain succeeded with tasks still running")
	}
	if !strings.Contains(err.Error(), "--drain=false") {
		t.Errorf("error should name the escape hatch, got: %v", err)
	}
	// And it must have rolled back rather than leaving the stack half-scaled.
	if sw.replicasOf("shop_app") != 2 || sw.replicasOf("shop_db") != 1 {
		t.Errorf("failed drain left the stack scaled: shop_app=%d shop_db=%d",
			sw.replicasOf("shop_app"), sw.replicasOf("shop_db"))
	}
}

// A stack that is not running at all is not an error: restore of a dead stack is
// the most common restore there is.
func TestDrainOfAStoppedStackIsANoOp(t *testing.T) {
	sw := newFakeSwarm(runtime.Service{ID: "s1", Name: "shop_app", Stack: "shop", Mode: "replicated", Desired: 0})
	rep, undo, err := (&Drainer{Scaler: sw}).Drain(context.Background(), "shop")
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rep.Paused) != 0 || len(sw.scaleLog) != 0 {
		t.Errorf("drained a stopped stack: paused=%v scales=%v", rep.Names(), sw.scaleLog)
	}
	undo() // must be safe
}

// Restore is the caller: it must pause before writing and resume afterwards,
// including when the restore fails.
func TestRestoreDrainsAndResumesAroundTheWrite(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	dir := t.TempDir()
	at := time.Now().UTC()
	name := ArtifactName("shop", "node1", at)
	writeTarGz(t, dir, name, "backup/data", map[string]string{"shop/data/x": "one"})

	if err := st.RecordDiscoveredBackupArtifact(ctx, name, filepath.Join(dir, name), at.Unix(), StatusSucceeded, "", "shop"); err != nil {
		t.Fatalf("record: %v", err)
	}
	var id int64
	rows, err := st.ListBackups(ctx, 5)
	if err != nil || len(rows) == 0 {
		t.Fatalf("ListBackups: %v", err)
	}
	id = rows[0].ID

	sw := shopStack()
	svc := &Local{Store: st, ArchiveDir: dir, Scaler: sw}
	if _, err := svc.Restore(ctx, id, filepath.Join(dir, "out"), RestoreOptions{Drain: true}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if sw.replicasOf("shop_app") != 2 || sw.replicasOf("shop_db") != 1 {
		t.Errorf("restore did not resume the stack: shop_app=%d shop_db=%d",
			sw.replicasOf("shop_app"), sw.replicasOf("shop_db"))
	}
	if len(sw.scaleLog) != 4 { // two services down, two back up
		t.Errorf("scale calls = %v, want 4 (2 down, 2 up)", sw.scaleLog)
	}

	// Restoring a whole-disk run has no stack to pause; and Drain=false must not
	// touch the swarm at all.
	sw2 := shopStack()
	svc2 := &Local{Store: st, ArchiveDir: dir, Scaler: sw2}
	if _, err := svc2.Restore(ctx, id, filepath.Join(dir, "out2"), RestoreOptions{Drain: false}); err != nil {
		t.Fatalf("Restore without drain: %v", err)
	}
	if len(sw2.scaleLog) != 0 {
		t.Errorf("--no-drain scaled services: %v", sw2.scaleLog)
	}
}

// A service with no runtime (nil Scaler) must still restore: the drain is a
// protection, not a dependency.
func TestRestoreWithoutAScalerStillRestores(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	dir := t.TempDir()
	at := time.Now().UTC()
	name := ArtifactName("shop", "node1", at)
	writeTarGz(t, dir, name, "backup/data", map[string]string{"shop/data/x": "one"})
	if err := st.RecordDiscoveredBackupArtifact(ctx, name, filepath.Join(dir, name), at.Unix(), StatusSucceeded, "", "shop"); err != nil {
		t.Fatalf("record: %v", err)
	}
	rows, _ := st.ListBackups(ctx, 5)
	svc := &Local{Store: st, ArchiveDir: dir}
	if _, err := svc.Restore(ctx, rows[0].ID, filepath.Join(dir, "out"), RestoreOptions{Drain: true}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "shop", "data", "x")); err != nil {
		t.Errorf("file not restored: %v", err)
	}
}
