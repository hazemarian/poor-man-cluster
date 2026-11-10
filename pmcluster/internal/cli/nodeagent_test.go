package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runners"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// fakeFetcher is the worker's half of the channel without a leader.
type fakeFetcher struct {
	mu         sync.Mutex
	calls      int
	a          runners.Assignment
	source     string
	err        error
	onCall     func(n int)
	perCallErr func(n int) error
}

func (f *fakeFetcher) Fetch(context.Context) (runners.Assignment, string, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if f.onCall != nil {
		f.onCall(n)
	}
	if f.perCallErr != nil {
		return runners.Assignment{}, "", f.perCallErr(n)
	}
	return f.a, f.source, f.err
}

func (f *fakeFetcher) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeLocalDocker is the node-local container runtime.
type fakeLocalDocker struct {
	mu         sync.Mutex
	containers map[string]runners.Container
	runCalls   [][]string
	removed    []string
}

func newFakeLocalDocker() *fakeLocalDocker {
	return &fakeLocalDocker{containers: map[string]runners.Container{}}
}

func (f *fakeLocalDocker) Run(_ context.Context, argv []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runCalls = append(f.runCalls, argv)
	name := ""
	for i, a := range argv {
		if a == "--name" && i+1 < len(argv) {
			name = argv[i+1]
		}
	}
	f.containers[name] = runners.Container{ID: "id-" + name, Name: name, State: "running"}
	return nil
}

func (f *fakeLocalDocker) Remove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, name)
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeLocalDocker) Inspect(_ context.Context, name string) (runners.Container, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[name]
	return c, ok, nil
}

func (f *fakeLocalDocker) Runs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.runCalls...)
}

// waitClosed blocks until ch closes, failing the test rather than hanging forever.
func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not happen", what)
	}
}

// workerFake is a Docker client that always reports a swarm WORKER: swarm active,
// no control access, and `docker node ls` failing the way it does on a worker.
func workerFake(t *testing.T) *leaderFake {
	t.Helper()
	return &leaderFake{
		info:    runtime.Info{SwarmLocalNodeState: "active", SwarmControlAvailable: false},
		nodeErr: errors.New("Error response from daemon: This node is not a swarm manager"),
	}
}

// TestWorkerLoopsStartBeforeLeadershipIsResolved is the regression test for the
// whole defect: the runner machinery and the filtered socket used to start AFTER
// waitForSwarmLeadership, which a worker never returns from — so the feature was
// inert on exactly the nodes allowed to host a runner.
func TestWorkerLoopsStartBeforeLeadershipIsResolved(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	fake := workerFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The premise: this client really is a worker, so the wait below really blocks.
	if !isSwarmWorker(ctx, fake) {
		t.Fatal("the fake does not look like a swarm worker; the test would prove nothing")
	}

	dir := t.TempDir()
	// A unix socket path the proxy can bind, and an upstream that exists, so the
	// proxy's first bind happens on this host (macOS has no /var/run/docker.sock).
	upstream := filepath.Join(dir, "docker.sock.upstream")
	if err := os.WriteFile(upstream, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	docker := newFakeLocalDocker()
	fetcher := &fakeFetcher{
		a: runners.Assignment{
			Node: host, Enabled: true, Org: "acme", Group: "pmcluster",
			SocketProxy: true, SocketPath: filepath.Join(dir, "filtered.sock"),
			Memory: "2560m", Mode: runners.ModeEphemeral, Labels: runners.Labels,
			RegistrationToken: "reg-token", Reason: "selected as a runner node",
		},
		source: "https://10.7.224.11:9092",
	}

	// A unix socket path has a ~100 byte limit, shorter than a macOS test temp
	// dir, so the listener's path gets its own short directory.
	short, err := os.MkdirTemp("", "pmc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	source := &socketPathSource{}
	source.setAssignment(filepath.Join(short, "f.sock"))
	loops := startPerNodeLoops(ctx, perNodeDeps{
		Worker:       true,
		Hostname:     host,
		Container:    docker,
		Assignment:   fetcher,
		StatePath:    filepath.Join(dir, "runner_assignment.json"),
		UpstreamPath: upstream,
		SocketPath:   source,
		Log:          zerolog.Nop(),
	})

	// The proxy bound synchronously, before anything asked about leadership.
	if !loops.Proxy.Running() {
		t.Fatal("the filtered docker socket was not listening when startPerNodeLoops returned")
	}

	// The runner half reports its first pass; no sleep, no polling loop.
	waitClosed(t, loops.FirstAssignment, "the worker's first assignment pass")
	if loops.Fetches() != 1 {
		t.Fatalf("assignment fetches = %d, want 1", loops.Fetches())
	}
	runs := docker.Runs()
	if len(runs) != 1 {
		t.Fatalf("docker run calls = %d, want the runner started from the assignment", len(runs))
	}
	if joined := strings.Join(runs[0], " "); strings.Contains(joined, "/var/run/docker.sock") {
		t.Fatalf("the worker's runner mounts the raw socket: %s", joined)
	}

	// NOW start the leadership wait. It must block, and the loops above have
	// already run — which is the ordering this test exists to pin.
	done := make(chan error, 1)
	go func() { done <- waitForSwarmLeadership(ctx, fake, zerolog.Nop()) }()
	select {
	case err := <-done:
		t.Fatalf("waitForSwarmLeadership returned (%v) on a worker; it must stand by", err)
	default:
	}
	if loops.Fetches() != 1 {
		t.Fatal("the assignment pass did not happen before the leadership wait started")
	}
	if err := loops.Proxy.Stop(); err != nil {
		t.Fatalf("stop the socket proxy: %v", err)
	}
}

// TestAPIBindAddresses pins the bind rule: the leader keeps what the operator
// configured (all interfaces), a worker answers on loopback only.
func TestAPIBindAddresses(t *testing.T) {
	cases := []struct {
		configured string
		worker     bool
		want       string
	}{
		{"0.0.0.0:9090", false, "0.0.0.0:9090"},
		{"0.0.0.0:9090", true, "127.0.0.1:9090"},
		{"10.0.0.5:9091", false, "10.0.0.5:9091"},
		{"10.0.0.5:9091", true, "127.0.0.1:9091"},
		{"9090", false, "0.0.0.0:9090"},
		{"9090", true, "127.0.0.1:9090"},
		{":9095", true, "127.0.0.1:9095"},
		{"", true, "127.0.0.1:9090"},
	}
	for _, tc := range cases {
		if got := apiAddr(tc.configured, tc.worker); got != tc.want {
			t.Errorf("apiAddr(%q, worker=%t) = %q, want %q", tc.configured, tc.worker, got, tc.want)
		}
	}
}

// TestIsStandby pins the rule behind the loopback bind: a worker always, a
// follower manager always, the leader never, and "cannot tell" never (a node that
// cannot prove leadership must not bind all interfaces).
func TestIsStandby(t *testing.T) {
	cases := []struct {
		name   string
		worker bool
		leader bool
		found  bool
		err    error
		want   bool
	}{
		{"worker", true, false, false, nil, true},
		{"follower manager", false, false, true, nil, true},
		{"leader", false, true, true, nil, false},
		{"manager not in the node list", false, false, false, nil, false},
		{"node list unreadable", false, false, false, errors.New("boom"), false},
	}
	for _, tc := range cases {
		if got := isStandby(tc.worker, tc.leader, tc.found, tc.err); got != tc.want {
			t.Errorf("%s: isStandby = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// TestAssignmentAddrIsTheSwarmDataPath: the mTLS listener binds the node's overlay
// address, never a public host port.
func TestAssignmentAddrIsTheSwarmDataPath(t *testing.T) {
	if got := assignmentAddr("10.7.224.11"); got != "10.7.224.11:9092" {
		t.Fatalf("assignmentAddr = %q", got)
	}
	if got := assignmentAddr("10.7.224.11:2377"); got != "10.7.224.11:9092" {
		t.Fatalf("assignmentAddr with a port = %q", got)
	}
	if got := assignmentAddr(""); got != "127.0.0.1:9092" {
		t.Fatalf("a node with no swarm address must bind loopback, got %q", got)
	}
}

// TestWorkerWithoutSwarmCertificateWarnsOnceAndStops proves the non-swarm box: one
// WRN per boot, no polling, and no unauthenticated fallback.
func TestWorkerWithoutSwarmCertificateWarnsOnceAndStops(t *testing.T) {
	logs := &logCapture{}
	fetcher := &fakeFetcher{err: runners.ErrNoSwarmCertificate}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	loops := startPerNodeLoops(ctx, perNodeDeps{
		Worker:     true,
		Hostname:   "w1",
		Container:  newFakeLocalDocker(),
		Assignment: fetcher,
		Interval:   5 * time.Millisecond,
		Log:        logs.logger(),
	})
	waitClosed(t, loops.FirstAssignment, "the first (failing) assignment pass")
	// The loop must STOP, not retry: with a 5ms interval it would have polled again
	// by the time this check runs.
	time.Sleep(50 * time.Millisecond)
	if got := fetcher.Calls(); got != 1 {
		t.Fatalf("assignment fetches = %d, want 1 (no polling without a swarm certificate)", got)
	}
	if got := strings.Count(logs.text(), "holds no swarm node certificate"); got != 1 {
		t.Fatalf("the warning appeared %d times, want exactly once:\n%s", got, logs.text())
	}
	if loops.Fetches() != 0 {
		t.Fatalf("successful fetches = %d, want 0", loops.Fetches())
	}
}

// TestFetchFailureKeepsLastStateAndWarnsOnce: a leader mid-failover must not tear a
// running runner down, and must not fill the journal either.
func TestFetchFailureKeepsLastStateAndWarnsOnce(t *testing.T) {
	logs := &logCapture{}
	docker := newFakeLocalDocker()
	// The runner is already running: the last applied state.
	_ = docker.Run(context.Background(), []string{"run", "-d", "--name", runners.RunnerName("w1"), "img"})

	calls := make(chan int, 16)
	fetcher := &fakeFetcher{err: errors.New("dial tcp 10.7.224.11:9092: connection refused")}
	fetcher.onCall = func(n int) {
		select {
		case calls <- n:
		default:
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	loops := startPerNodeLoops(ctx, perNodeDeps{
		Worker:     true,
		Hostname:   "w1",
		Container:  docker,
		Assignment: fetcher,
		Interval:   5 * time.Millisecond,
		Log:        logs.logger(),
	})
	waitClosed(t, loops.FirstAssignment, "the first (failing) assignment pass")

	// Drive a few more passes without sleeping for state to settle.
	for i := 0; i < 3; i++ {
		select {
		case <-calls:
		case <-time.After(2 * time.Second):
			t.Fatalf("the poll loop stopped after %d calls", fetcher.Calls())
		}
	}
	cancel()

	if _, ok, _ := docker.Inspect(context.Background(), runners.RunnerName("w1")); !ok {
		t.Fatal("a failed assignment fetch removed the running runner; the last state must be kept")
	}
	if n := len(docker.Runs()); n != 1 {
		t.Fatalf("docker run calls = %d, want 1 (no churn while the leader is unreachable)", n)
	}
	if out := logs.text(); strings.Count(out, "could not read this node's runner assignment") != 1 {
		t.Fatalf("the warning was not bounded to one line:\n%s", out)
	}
}

// TestSocketPathSourceFallbacks: the path comes from the store when there is one
// (a manager), from the assignment on a worker, and from the platform default
// before either has been read.
func TestSocketPathSourceFallbacks(t *testing.T) {
	s := &socketPathSource{}
	if got := s.Path(context.Background()); got != "/run/pmcluster/docker.sock" {
		t.Fatalf("default path = %q", got)
	}
	s.setAssignment("/run/from-assignment.sock")
	if got := s.Path(context.Background()); got != "/run/from-assignment.sock" {
		t.Fatalf("assignment path = %q", got)
	}
}

// TestManagerHostsFromDockerInfo: a worker learns the manager addresses from the
// swarm itself, which is the whole point of not needing DNS or a published port.
func TestManagerHostsFromDockerInfo(t *testing.T) {
	f := &leaderFake{info: runtime.Info{
		SwarmRemoteManagers: []string{"10.7.224.11:2377", "10.7.224.12:2377"},
	}}
	hosts, err := managerHosts(context.Background(), f)
	if err != nil {
		t.Fatalf("managerHosts: %v", err)
	}
	if len(hosts) != 2 || hosts[0] != "10.7.224.11" || hosts[1] != "10.7.224.12" {
		t.Fatalf("hosts = %v", hosts)
	}
	// A node with no manager list falls back to its own swarm address, and an
	// unreachable docker is an error (never an empty success).
	single := &leaderFake{info: runtime.Info{SwarmNodeAddr: "10.7.224.11"}}
	if hosts, _ := managerHosts(context.Background(), single); len(hosts) != 1 || hosts[0] != "10.7.224.11" {
		t.Fatalf("single-manager hosts = %v", hosts)
	}
	if _, err := managerHosts(context.Background(), nil); err == nil {
		t.Fatal("no docker client must be an error")
	}
}

// TestAgentStatePathIsInTheDataDir keeps the doctor and the daemon agreeing on
// where the applied assignment is recorded.
func TestAgentStatePathIsInTheDataDir(t *testing.T) {
	if got := agentStatePath("/root/.pmcluster"); got != "/root/.pmcluster/runner_assignment.json" {
		t.Fatalf("agentStatePath = %q", got)
	}
	if got := agentStatePath(""); got != "" {
		t.Fatalf("empty data dir → %q", got)
	}
}

// TestWorkerRecordIsReadableByTheDoctor cross-checks the two halves of the
// assignment cache: what the agent writes is what the doctor parses.
func TestWorkerRecordIsReadableByTheDoctor(t *testing.T) {
	dir := t.TempDir()
	path := agentStatePath(dir)
	agent := &runners.Agent{Node: "nxt-sw-6-w", Docker: newFakeLocalDocker(), StatePath: path, Log: zerolog.Nop()}
	if _, err := agent.Apply(context.Background(), runners.Assignment{
		Node: "nxt-sw-6-w", Enabled: true, Org: "acme", Group: "pmcluster",
		SocketProxy: true, SocketPath: "/run/pmcluster/docker.sock",
		Memory: "2560m", RegistrationToken: "t",
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	agent.RecordFetch("https://10.7.224.11:9092", time.Now())
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec runners.AgentRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("the doctor could not parse the agent state: %v", err)
	}
	if rec.Source == "" || rec.FetchedAt == 0 || rec.Container != "running" || rec.Org != "acme" {
		t.Fatalf("record = %+v", rec)
	}
}
