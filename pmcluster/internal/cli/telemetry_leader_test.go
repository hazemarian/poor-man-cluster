package cli

import (
	"context"
	"errors"
	"testing"
)

// TestCollectorEndpointForPicksTheManagerThatAnswers pins the discovery rule: only the
// leader publishes the collector port, and a worker cannot be told which manager that is,
// so the first manager that ANSWERS is the leader. No dial, no endpoint.
func TestCollectorEndpointForPicksTheManagerThatAnswers(t *testing.T) {
	hosts := func(context.Context) ([]string, error) {
		return []string{"10.7.224.12:2377", "10.7.224.11:2377", "10.7.224.12"}, nil
	}

	var tried []string
	dial := func(_ context.Context, target string) bool {
		tried = append(tried, target)
		return target == "10.7.224.11:4318"
	}
	got := collectorEndpointFor(context.Background(), hosts, dial)
	if got != "http://10.7.224.11:4318" {
		t.Fatalf("endpoint = %q, want the only manager that answered", got)
	}
	// The port comes off and the collector's port goes on; a duplicate address is asked
	// once, not three times.
	want := []string{"10.7.224.12:4318", "10.7.224.11:4318"}
	if len(tried) != len(want) || tried[0] != want[0] || tried[1] != want[1] {
		t.Fatalf("dialled %v, want %v (duplicates folded, ports re-addressed)", tried, want)
	}
}

// TestCollectorEndpointForReturnsNothingWhenNobodyAnswers: the caller must disable the
// exporter rather than hand it an address nobody listens on.
func TestCollectorEndpointForReturnsNothingWhenNobodyAnswers(t *testing.T) {
	hosts := func(context.Context) ([]string, error) { return []string{"10.7.224.11"}, nil }
	if got := collectorEndpointFor(context.Background(), hosts, func(context.Context, string) bool { return false }); got != "" {
		t.Fatalf("endpoint = %q, want empty when nothing answers", got)
	}
	// A swarm read that fails is also "nothing", never a panic or a partial URL.
	broken := func(context.Context) ([]string, error) { return nil, errors.New("no docker") }
	if got := collectorEndpointFor(context.Background(), broken, func(context.Context, string) bool { return true }); got != "" {
		t.Fatalf("endpoint = %q, want empty when the manager list is unreadable", got)
	}
}
