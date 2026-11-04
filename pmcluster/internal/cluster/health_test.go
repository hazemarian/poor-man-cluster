package cluster

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

func TestWaitHealthyStacks_AllHealthy(t *testing.T) {
	f := newFakeDocker()
	for _, name := range bundledServices {
		f.services[name] = runtime.Service{Name: name, Replicas: 1, Desired: 1}
	}
	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := WaitHealthyStacks(ctx, f, &buf); err != nil {
		t.Fatalf("WaitHealthyStacks: %v", err)
	}
}

func TestWaitHealthyStacks_TimesOutWithMissingService(t *testing.T) {
	f := newFakeDocker()

	// Register every bundled service EXCEPT the last one: indexing off the list
	// made this test silently stop testing anything the moment the list shrank.
	for _, name := range bundledServices[:len(bundledServices)-1] {
		f.services[name] = runtime.Service{Name: name, Replicas: 1, Desired: 1}
	}
	if len(bundledServices) < 2 {
		t.Fatal("bundledServices is too short to have a missing one")
	}
	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := WaitHealthyStacks(ctx, f, &buf)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestWaitHealthyStacks_TimesOutWithUnhealthyReplicas(t *testing.T) {
	f := newFakeDocker()
	for _, name := range bundledServices {
		f.services[name] = runtime.Service{Name: name, Replicas: 0, Desired: 1}
	}
	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := WaitHealthyStacks(ctx, f, &buf)
	if err == nil {
		t.Fatal("expected timeout error due to 0/1 replicas, got nil")
	}
}
