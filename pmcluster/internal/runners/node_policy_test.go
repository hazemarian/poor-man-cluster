package runners

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
)

// TestApplyRecordsTheNodePolicyEvenWhenNothingStarts is the regression for a silent
// no-op: the policy was written beside the container start, so a node whose runner was
// already healthy returned early and never recorded the leader's answer. That is the
// normal state of a runner node, which is why the switch looked broken on the live
// fleet while every unit test passed.
func TestApplyRecordsTheNodePolicyEvenWhenNothingStarts(t *testing.T) {
	dir := t.TempDir()
	d := newFakeDocker()
	name := RunnerName("worker-1")
	// The runner already exists and is healthy: Apply must apply nothing.
	d.containers[name] = Container{ID: "id-" + name, Name: name, State: "running"}

	a := &Agent{
		Node:      "worker-1",
		Docker:    d,
		StatePath: filepath.Join(dir, "runner_assignment.json"),
		Log:       zerolog.Nop(),
	}
	if _, err := a.Apply(context.Background(), Assignment{
		Node:              "worker-1",
		Enabled:           true,
		Org:               "acme",
		Group:             "pmcluster",
		Memory:            "2560m",
		RegistrationToken: "tok",
		NodeDockerRestart: true,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(d.runCalls) != 0 {
		t.Fatalf("a healthy runner started %d container(s); this test is about the early return", len(d.runCalls))
	}
	if got := ReadNodePolicy(dir); !got.DockerRestart {
		t.Fatalf("node policy = %+v, want DockerRestart true: the watchdog never learns the leader's answer", got)
	}
}

// TestApplyClearsTheNodePolicyWhenDisabled: turning the switch off must actually disarm
// the watchdog on the next assignment, not only on the next daemon restart.
func TestApplyClearsTheNodePolicyWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	if err := WriteNodePolicy(dir, NodePolicy{DockerRestart: true}); err != nil {
		t.Fatal(err)
	}
	d := newFakeDocker()
	name := RunnerName("worker-1")
	d.containers[name] = Container{ID: "id-" + name, Name: name, State: "running"}

	a := &Agent{
		Node:      "worker-1",
		Docker:    d,
		StatePath: filepath.Join(dir, "runner_assignment.json"),
		Log:       zerolog.Nop(),
	}
	if _, err := a.Apply(context.Background(), Assignment{
		Node:              "worker-1",
		Enabled:           true,
		Org:               "acme",
		Group:             "pmcluster",
		Memory:            "2560m",
		RegistrationToken: "tok",
		NodeDockerRestart: false,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := ReadNodePolicy(dir); got.DockerRestart {
		t.Fatalf("node policy = %+v, want DockerRestart false after the operator turned it off", got)
	}
}
