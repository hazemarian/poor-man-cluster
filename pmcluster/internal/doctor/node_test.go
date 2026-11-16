package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runners"
)

// fakeRunnerDocker is the node-local container surface.
type fakeRunnerDocker struct {
	container runners.Container
	exists    bool
	err       error
}

func (f fakeRunnerDocker) Run(context.Context, []string) error { return nil }
func (f fakeRunnerDocker) Remove(context.Context, string) error {
	return nil
}
func (f fakeRunnerDocker) Inspect(context.Context, string) (runners.Container, bool, error) {
	return f.container, f.exists, f.err
}

func find(report *Report, name string) (Check, bool) {
	for _, c := range report.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

// TestRunNodeLocalReportsTheWorkerFields is the requirement: four measurements a
// worker can make, and an explicit line per cluster question it will not answer.
func TestRunNodeLocalReportsTheWorkerFields(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "docker.sock")
	// A real unix socket file is not needed: the check reports presence and type,
	// so a regular file is the honest "exists but is not a socket" case below.
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "runner_assignment.json")
	rec := runners.AgentRecord{
		Node: "nxt-sw-6-w", Enabled: true, Org: "acme", Group: "pmcluster",
		RunnerName: "pmc-nxt-sw-6-w-org", Container: "running",
		FetchedAt: time.Now().Add(-30 * time.Second).Unix(),
		AppliedAt: time.Now().Unix(),
		Source:    "https://10.7.224.11:9092",
	}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(statePath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	rep := RunNodeLocal(context.Background(), NodeInput{
		Hostname:   "nxt-sw-6-w",
		ListenAddr: "127.0.0.1:9090",
		SocketPath: socket,
		StatePath:  statePath,
		VolumeRoot: dir,
		Docker:     fakeRunnerDocker{exists: true, container: runners.Container{Name: "pmc-nxt-sw-6-w-org", State: "running"}},
		Health:     func(context.Context, string) (int, error) { return http.StatusOK, nil },
	})

	daemon, ok := find(rep, "daemon")
	if !ok || daemon.Status != StatusOK {
		t.Fatalf("daemon check = %+v ok=%v", daemon, ok)
	}
	if !strings.Contains(daemon.Detail, "127.0.0.1:9090") {
		t.Fatalf("the daemon check should name the address it probed: %q", daemon.Detail)
	}
	// There is no socket check any more: a job brings its own daemon, so a missing
	// host socket is not a warning — it is the normal state of a node that runs jobs.
	if sock, ok := find(rep, "socket proxy"); ok {
		t.Fatalf("doctor still reports a socket-proxy check: %+v", sock)
	}
	assignment, ok := find(rep, "runner assignment")
	if !ok || assignment.Status != StatusOK {
		t.Fatalf("assignment check = %+v ok=%v", assignment, ok)
	}
	for _, want := range []string{"ago", "https://10.7.224.11:9092", "enabled=true", "acme", "pmc-nxt-sw-6-w-org"} {
		if !strings.Contains(assignment.Detail, want) {
			t.Fatalf("the assignment check must report age and source: missing %q in %q", want, assignment.Detail)
		}
	}
	runner, ok := find(rep, "local runner")
	if !ok || runner.Status != StatusOK || !strings.Contains(runner.Detail, "running") {
		t.Fatalf("local runner check = %+v ok=%v", runner, ok)
	}
	// The cluster questions it cannot answer are named, not silently omitted.
	cluster, ok := find(rep, "cluster state")
	if !ok || cluster.Status != StatusSkip {
		t.Fatalf("cluster state check = %+v ok=%v", cluster, ok)
	}
	if !strings.Contains(cluster.Detail, "leader") {
		t.Fatalf("the skipped cluster check must say where to ask: %q", cluster.Detail)
	}
	if _, ok := find(rep, "swarm"); !ok {
		t.Fatal("the worker report must say why quorum is not answered here")
	}
}

// TestRunNodeLocalFailsWhenTheDaemonIsNotListening is the defect this whole change
// exists for, expressed as a check: nothing listening on the node's own API.
func TestRunNodeLocalFailsWhenTheDaemonIsNotListening(t *testing.T) {
	rep := RunNodeLocal(context.Background(), NodeInput{
		Hostname:   "nxt-sw-6-w",
		ListenAddr: "0.0.0.0:9090",
		VolumeRoot: t.TempDir(),
		Docker:     fakeRunnerDocker{},
		Health: func(context.Context, string) (int, error) {
			return 0, errors.New("connection refused")
		},
	})
	daemon, ok := find(rep, "daemon")
	if !ok || daemon.Status != StatusFail {
		t.Fatalf("daemon check = %+v ok=%v, want a failure", daemon, ok)
	}
	if !strings.Contains(daemon.Detail, "systemctl status pmcluster") {
		t.Fatalf("the failure should say what to check: %q", daemon.Detail)
	}
	if !strings.Contains(daemon.Detail, "127.0.0.1:9090") {
		t.Fatalf("a worker binds loopback, so the probe must be loopback: %q", daemon.Detail)
	}
	if rep.OK {
		t.Fatal("a report with a failing check must not be OK")
	}
}

// TestRunNodeLocalAssignmentStates covers "never fetched", "stale" and "last apply
// failed" — the three answers an operator needed and could not get on a worker.
func TestRunNodeLocalAssignmentStates(t *testing.T) {
	base := NodeInput{Hostname: "w1", VolumeRoot: t.TempDir(), Docker: fakeRunnerDocker{},
		Health: func(context.Context, string) (int, error) { return http.StatusOK, nil }}

	t.Run("never fetched", func(t *testing.T) {
		in := base
		in.StatePath = filepath.Join(t.TempDir(), "absent.json")
		c, _ := find(RunNodeLocal(context.Background(), in), "runner assignment")
		if c.Status != StatusSkip || !strings.Contains(c.Detail, "never fetched") {
			t.Fatalf("assignment check = %+v", c)
		}
		if !strings.Contains(c.Detail, "swarm-certificate") {
			t.Fatalf("the detail should point at the likely cause: %q", c.Detail)
		}
	})

	t.Run("stale", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "a.json")
		b, _ := json.Marshal(runners.AgentRecord{
			Enabled: false, Source: "https://10.0.0.1:9092",
			FetchedAt: time.Now().Add(-30 * time.Minute).Unix(),
		})
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		in := base
		in.StatePath = path
		c, _ := find(RunNodeLocal(context.Background(), in), "runner assignment")
		if c.Status != StatusWarn || !strings.Contains(c.Detail, "stale") {
			t.Fatalf("assignment check = %+v", c)
		}
	})

	t.Run("last apply error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "a.json")
		b, _ := json.Marshal(runners.AgentRecord{
			Enabled: true, Org: "acme", LastError: "docker: no such image",
			FetchedAt: time.Now().Unix(),
		})
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		in := base
		in.StatePath = path
		c, _ := find(RunNodeLocal(context.Background(), in), "runner assignment")
		if c.Status != StatusWarn || !strings.Contains(c.Detail, "no such image") {
			t.Fatalf("assignment check = %+v", c)
		}
	})
}

// TestRunNodeLocalRawSocketIsAWarning: the debug fallback must be visible on the
// node that is running it.
func TestRunNodeLocalRawSocketIsAWarning(t *testing.T) {
	rep := RunNodeLocal(context.Background(), NodeInput{
		Hostname:   "w1",
		VolumeRoot: t.TempDir(),
		Docker: fakeRunnerDocker{exists: true, container: runners.Container{
			Name: "pmc-w1-org", State: "running",
			Binds: []string{"/var/run/docker.sock:/var/run/docker.sock"},
		}},
		Health: func(context.Context, string) (int, error) { return http.StatusOK, nil },
	})
	c, _ := find(rep, "local runner")
	if c.Status != StatusWarn || !strings.Contains(c.Detail, "RAW") {
		t.Fatalf("local runner check = %+v", c)
	}
}

// ExecIn satisfies runners.LocalDocker: the doctor never probes a dind daemon, so
// this answers as if one were ready.
func (f fakeRunnerDocker) ExecIn(context.Context, string, ...string) ([]byte, error) {
	return []byte("ok"), nil
}

// RemoveVolume satisfies runners.LocalDocker for the same reason.
func (f fakeRunnerDocker) RemoveVolume(context.Context, string) error { return nil }

// VolumeSizes: this fake models a node with no cache volumes unless a test sets them.
func (d fakeRunnerDocker) VolumeSizes(_ context.Context, prefix string) ([]runners.VolumeSize, error) {
	return nil, nil
}
