package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestTransientDeployError pins the two answers that mean "another writer touched this stack", and —
// just as important — that a real failure is NOT one of them. A retry loop that swallows a genuine
// error hides it behind a second, worse message.
func TestTransientDeployError(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"rpc error: code = Unknown desc = update out of sequence", true},
		{"failed to create service loop-drift_web: Error response from daemon: rpc error: code = " +
			"AlreadyExists desc = name conflicts with an existing object: service loop-drift_web " +
			"already exists", true},
		{"failed to create service x: no suitable node (insufficient resources)", false},
		{"Error response from daemon: network not found", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := transientDeployError(tc.out); got != tc.want {
			t.Errorf("transientDeployError(%.60q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

// TestDeployStack_RetriesWhenAnotherWriterWonTheRace — the measured CI failure: a reconcile pass and
// an operator's deploy ran in the same second, `docker stack deploy` created the service, and the
// second writer's list-then-create hit "name conflicts with an existing object" for a service that
// existed and only needed an update. The deploy must survive that, because the stack is fine.
func TestDeployStack_RetriesWhenAnotherWriterWonTheRace(t *testing.T) {
	var deploys int
	d := &dockerCLIDeployer{
		run: func(ctx context.Context, stdin []byte, args ...string) (string, error) {
			if len(args) >= 2 && args[0] == "stack" && args[1] == "deploy" {
				deploys++
				if deploys == 1 {
					return "failed to create service loop-drift_web: Error response from daemon: " +
						"rpc error: code = AlreadyExists desc = name conflicts with an existing " +
						"object: service loop-drift_web already exists", errors.New("exit status 1")
				}
				return "Creating service loop-drift_web", nil
			}
			return "", nil // the mode-reconciliation probes
		},
	}
	if err := d.deployStack(context.Background(), "loop-drift", []byte("services: {}\n")); err != nil {
		t.Fatalf("deployStack returned %v; a name conflict with another writer must be retried", err)
	}
	if deploys != 2 {
		t.Errorf("docker stack deploy ran %d times, want 2 (fail, then retry)", deploys)
	}
}

// TestDeployStack_DoesNotRetryARealFailure — the retry must not be a blanket "try again": an error
// that has nothing to do with a concurrent writer is reported on the first attempt, with Docker's own
// words in it.
func TestDeployStack_DoesNotRetryARealFailure(t *testing.T) {
	var deploys int
	d := &dockerCLIDeployer{
		run: func(ctx context.Context, stdin []byte, args ...string) (string, error) {
			if len(args) >= 2 && args[0] == "stack" && args[1] == "deploy" {
				deploys++
				return "failed to create service x: no suitable node (insufficient resources)", errors.New("exit status 1")
			}
			return "", nil
		},
	}
	err := d.deployStack(context.Background(), "loop-drift", []byte("services: {}\n"))
	if err == nil {
		t.Fatal("deployStack swallowed a real failure")
	}
	if deploys != 1 {
		t.Errorf("docker stack deploy ran %d times, want 1: a resource error must not be retried", deploys)
	}
	if !strings.Contains(err.Error(), "no suitable node") {
		t.Errorf("the error lost Docker's words: %v", err)
	}
}
