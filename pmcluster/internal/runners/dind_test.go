package runners

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// testSpec is the DinD runner the tests exercise: the smallest spec that renders.
func testSpec() Spec {
	return Spec{
		Org:        "acme",
		Group:      "pmcluster",
		RunnerName: "pmc-n1-org",
		Image:      DefaultImage,
		Memory:     "2560m",
		Token:      "tok",
	}
}

// TestDinDArgvGivesTheJobItsOwnDaemon asserts the container that makes compose work
// and the exposure that pays for it: privileged, its own socket volume, no host
// socket anywhere in the vector.
func TestDinDArgvGivesTheJobItsOwnDaemon(t *testing.T) {
	s := testSpec()
	argv, err := s.DinDArgv()
	if err != nil {
		t.Fatalf("DinDArgv: %v", err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{
		"--privileged",
		"--name " + s.DinDName(),
		"-v " + s.DinDVolume() + ":/var/run",
		"DOCKER_TLS_CERTDIR=",
		"--memory 2560m",
		"pmcluster.dind=true",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("dind argv is missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "/var/run/docker.sock:") {
		t.Fatalf("the host socket is bind-mounted into the dind container:\n%s", joined)
	}
	// The daemon container must not run the RUNNER image. Not hypothetical: the first
	// version fell back to Spec.Image, so the privileged container booted a runner
	// with no token, exited at once, and the reconcile reported "the dind daemon in
	// <name> never answered".
	if got := argv[len(argv)-1]; got != DefaultDinDImage {
		t.Fatalf("the dind container runs %q, want %q", got, DefaultDinDImage)
	}
	if strings.Contains(joined, DefaultImage) {
		t.Fatalf("the dind container is running the runner image:\n%s", joined)
	}
}

// TestDinDRunnerUsesItsOwnSocketNotTheHosts is the security assertion: the runner
// talks to the DinD daemon through the shared volume, and the host's socket is
// never mounted. A job may own its own daemon; it may not own the node's.
func TestDinDRunnerUsesItsOwnSocketNotTheHosts(t *testing.T) {
	s := testSpec()
	argv, err := s.Argv()
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "-v "+s.DinDVolume()+":/var/run") {
		t.Fatalf("the runner does not share the dind socket volume:\n%s", joined)
	}
	if !strings.Contains(joined, "DOCKER_HOST=unix:///var/run/docker.sock") {
		t.Fatalf("DOCKER_HOST does not point at the inner daemon:\n%s", joined)
	}
	if strings.Contains(joined, "/var/run/docker.sock:/var/run/docker.sock") {
		t.Fatalf("the host socket is mounted into the runner:\n%s", joined)
	}
	if strings.Contains(joined, "/run/pmcluster") {
		t.Fatalf("a dind runner still mounts the platform socket proxy:\n%s", joined)
	}
}

// TestStartRunnerStartsTheDaemonBeforeTheRunner pins the order. A runner started
// into a daemon that is not listening yet fails for reasons invisible from the
// workflow, so the daemon is created first and waited for.
func TestStartRunnerStartsTheDaemonBeforeTheRunner(t *testing.T) {
	d := newFakeDocker()
	s := testSpec()
	if err := StartRunner(context.Background(), d, s); err != nil {
		t.Fatalf("StartRunner: %v", err)
	}
	if len(d.runCalls) != 2 {
		t.Fatalf("expected the dind container then the runner, got %d run call(s)", len(d.runCalls))
	}
	if !strings.Contains(strings.Join(d.runCalls[0], " "), s.DinDName()) {
		t.Fatalf("the first container started is not the dind one: %v", d.runCalls[0])
	}
	if !strings.Contains(strings.Join(d.runCalls[1], " "), s.RunnerName) {
		t.Fatalf("the second container started is not the runner: %v", d.runCalls[1])
	}
	if len(d.execCalls) == 0 {
		t.Fatal("the daemon was never asked whether it was ready before the runner started")
	}
}

// TestStartRunnerRefusesToStartARunnerWithoutADaemon: if the inner daemon never
// answers, the runner must not be started, and the privileged container must be
// removed rather than left holding the node.
func TestStartRunnerRefusesToStartARunnerWithoutADaemon(t *testing.T) {
	old := dindWaitTimeout
	t.Cleanup(func() { dindWaitTimeout = old })
	dindWaitTimeout = 150 * time.Millisecond

	d := newFakeDocker()
	d.execErr = errors.New("cannot connect to the Docker daemon")
	s := testSpec()
	err := StartRunner(context.Background(), d, s)
	if err == nil {
		t.Fatal("StartRunner succeeded with a daemon that never answered")
	}
	// The runner container's name is the runner name exactly; the dind container is
	// "<name>-dind", so a substring match would flag the daemon itself.
	for _, argv := range d.runCalls {
		for i, a := range argv {
			if a == "--name" && i+1 < len(argv) && argv[i+1] == s.RunnerName {
				t.Fatalf("a runner was started without a working daemon: %v", argv)
			}
		}
	}
	if len(d.removed) == 0 {
		t.Fatal("the dind container was left behind after the daemon failed to start")
	}
}

// TestStopRunnerRemovesEverythingTheJobCreated is the "when the job is done the
// whole runner is removed" contract: runner, daemon, and the volume that holds the
// job's images. A surviving volume is a node that fills up one job at a time.
func TestStopRunnerRemovesEverythingTheJobCreated(t *testing.T) {
	d := newFakeDocker()
	s := testSpec()
	if err := StopRunner(context.Background(), d, s.RunnerName); err != nil {
		t.Fatalf("StopRunner: %v", err)
	}
	wantRemoved := []string{s.RunnerName, s.DinDName()}
	if strings.Join(d.removed, ",") != strings.Join(wantRemoved, ",") {
		t.Fatalf("removed %v, want %v (runner first, then the daemon)", d.removed, wantRemoved)
	}
	if len(d.volumesRemoved) != 1 || d.volumesRemoved[0] != s.DinDVolume() {
		t.Fatalf("volumes removed = %v, want [%s]", d.volumesRemoved, s.DinDVolume())
	}
}

// TestStopRunnerIsIdempotent: a crashed job leaves the names behind and the
// reconcile retries, so teardown of things that do not exist must be success.
func TestStopRunnerIsIdempotent(t *testing.T) {
	d := newFakeDocker()
	if err := StopRunner(context.Background(), d, ""); err != nil {
		t.Fatalf("StopRunner with an empty name: %v", err)
	}
	if len(d.removed) != 0 || len(d.volumesRemoved) != 0 {
		t.Fatalf("an empty name removed something: %v %v", d.removed, d.volumesRemoved)
	}
}

// TestThereIsNoNonDinDPath: the socket modes were removed, not disabled. A spec must
// always produce the pair, and nothing may mount the platform's old filtered socket.
func TestThereIsNoNonDinDPath(t *testing.T) {
	s := testSpec()
	d := newFakeDocker()
	if err := StartRunner(context.Background(), d, s); err != nil {
		t.Fatalf("StartRunner: %v", err)
	}
	if len(d.runCalls) != 2 {
		t.Fatalf("a runner started %d containers, want 2 (the daemon and the runner)", len(d.runCalls))
	}
	for _, argv := range d.runCalls {
		if strings.Contains(strings.Join(argv, " "), "/run/pmcluster") {
			t.Fatalf("a container still mounts the removed filtered socket:\n%s", strings.Join(argv, " "))
		}
	}
}

// TestRemoveVolumeTreatsBothMissingWordingsAsSuccess is the regression for the first
// start on a fresh node: `docker volume rm` on a volume that was never created answers
// "volume X not found", while `volume inspect` says "no such volume". Only the second
// was recognised, so StopRunner failed before the runner ever started, with an error that
// reads like a broken teardown.
func TestRemoveVolumeTreatsBothMissingWordingsAsSuccess(t *testing.T) {
	for _, msg := range []string{
		"Error response from daemon: volume pmc-n1-org-docker not found",
		"Error response from daemon: get pmc-n1-org-docker: no such volume",
		"Error: No such volume: pmc-n1-org-docker",
	} {
		// The argument check is first: the empty-name guard must not be what makes this
		// pass.
		if err := roundTripRemoveVolume(t, msg); err != nil {
			t.Fatalf("%q was treated as a real failure: %v", msg, err)
		}
	}
	// A volume still IN USE is a genuine ordering bug — containers must go first — and
	// must stay visible.
	if err := roundTripRemoveVolume(t, "Error response from daemon: remove pmc-n1-org-docker: volume is in use - [abc]"); err == nil {
		t.Fatal("a volume in use was reported as success")
	}
}

func roundTripRemoveVolume(t *testing.T, daemonSays string) error {
	t.Helper()
	d := &CLIDocker{Exec: func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New(daemonSays)
	}}
	return d.RemoveVolume(context.Background(), "pmc-n1-org-docker")
}

// TestTheDataDirectorySurvivesButStartsClean pins both halves of a decision that is easy
// to get half-right: /var/lib/docker is mounted so the next job reuses this job's layers
// and build cache, and the previous job's stopped containers are pruned so it does not
// accumulate them forever.
func TestTheDataDirectorySurvivesButStartsClean(t *testing.T) {
	d := newFakeDocker()
	s := testSpec()
	if err := StartRunner(context.Background(), d, s); err != nil {
		t.Fatalf("StartRunner: %v", err)
	}
	dind := strings.Join(d.runCalls[0], " ")
	if !strings.Contains(dind, s.DataVolume()+":/var/lib/docker") {
		t.Fatalf("the daemon does not keep its data directory (no layer or build-cache reuse):\n%s", dind)
	}
	// The socket volume is still per-job.
	if !strings.Contains(dind, s.DinDVolume()+":/var/run") {
		t.Fatalf("the daemon does not share its socket volume:\n%s", dind)
	}
	var pruned []string
	for _, call := range d.execCalls {
		pruned = append(pruned, strings.Join(call, " "))
	}
	joined := strings.Join(pruned, "|")
	for _, want := range []string{"container prune", "image prune", "volume prune", "builder prune"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the daemon is not pruned before the job (%q missing): %v", want, pruned)
		}
	}
	if !strings.Contains(joined, "--keep-storage") {
		t.Fatalf("the build cache has no ceiling: %v", pruned)
	}
}
