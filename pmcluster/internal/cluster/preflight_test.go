package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/runtime"
)

func TestPreflight_PingFails(t *testing.T) {
	f := newFakeDocker()
	f.pingErr = errSentinel
	f.info = goodSwarmInfo()

	err := Preflight(context.Background(), f)
	if err == nil {
		t.Fatal("Preflight: expected error when Ping fails, got nil")
	}
	var pe *PreflightError
	if !errors.As(err, &pe) {
		t.Fatalf("err is %T, want *PreflightError", err)
	}
	if !errors.Is(pe.Cause, errSentinel) {
		t.Errorf("Cause = %v, want errSentinel", pe.Cause)
	}
	if !strings.Contains(pe.Remediation, "Docker daemon") {
		t.Errorf("Remediation = %q, want mention of 'Docker daemon'", pe.Remediation)
	}
}

func TestPreflight_InfoFails(t *testing.T) {
	f := newFakeDocker()

	f.infoErr = errSentinel

	err := Preflight(context.Background(), f)
	if err == nil {
		t.Fatal("expected error when Info fails, got nil")
	}
	var pe *PreflightError
	if !errors.As(err, &pe) {
		t.Fatalf("err is %T, want *PreflightError", err)
	}
	if !errors.Is(pe.Cause, errSentinel) {
		t.Errorf("Cause = %v, want errSentinel", pe.Cause)
	}
}

func TestPreflight_SwarmNotActive(t *testing.T) {
	cases := []struct {
		state string
	}{
		{"inactive"},
		{"pending"},
		{"error"},
		{"locked"},
	}

	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			f := newFakeDocker()
			f.info = runtime.Info{
				SwarmLocalNodeState:   tc.state,
				SwarmControlAvailable: true,
			}

			err := Preflight(context.Background(), f)
			if err == nil {
				t.Fatalf("state=%q: expected error, got nil", tc.state)
			}
			var pe *PreflightError
			if !errors.As(err, &pe) {
				t.Fatalf("err is %T, want *PreflightError", err)
			}
			if !strings.Contains(pe.Remediation, "docker swarm init") {
				t.Errorf("Remediation = %q, want mention of 'docker swarm init'", pe.Remediation)
			}
		})
	}
}

func TestPreflight_WorkerNode(t *testing.T) {
	f := newFakeDocker()
	f.info = runtime.Info{
		SwarmLocalNodeState:   "active",
		SwarmControlAvailable: false,
	}

	err := Preflight(context.Background(), f)
	if err == nil {
		t.Fatal("expected error for worker node, got nil")
	}
	var pe *PreflightError
	if !errors.As(err, &pe) {
		t.Fatalf("err is %T, want *PreflightError", err)
	}
	if !strings.Contains(pe.Remediation, "manager") {
		t.Errorf("Remediation = %q, want mention of 'manager'", pe.Remediation)
	}
}

func TestPreflight_HappyPath(t *testing.T) {
	f := newFakeDocker()
	f.info = goodSwarmInfo()

	err := Preflight(context.Background(), f)
	if err != nil {
		t.Fatalf("Preflight happy path: %v", err)
	}
}

func TestPreflightError_CarriesCauseAndRemediation(t *testing.T) {
	cause := errors.New("root cause")
	pe := &PreflightError{
		Cause:       cause,
		Remediation: "do this to fix it",
	}

	msg := pe.Error()
	if !strings.Contains(msg, "root cause") {
		t.Errorf("Error() = %q, missing 'root cause'", msg)
	}
	if !strings.Contains(msg, "do this to fix it") {
		t.Errorf("Error() = %q, missing remediation", msg)
	}

	if !errors.Is(pe, cause) {
		t.Error("errors.Is(pe, cause) = false, want true")
	}
}

func TestPreflightError_NilCause(t *testing.T) {
	pe := &PreflightError{Cause: nil, Remediation: "fix this"}
	if pe.Error() != "fix this" {
		t.Errorf("Error() = %q, want %q", pe.Error(), "fix this")
	}
	if pe.Unwrap() != nil {
		t.Errorf("Unwrap() = %v, want nil", pe.Unwrap())
	}
}
