package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/engine"
)

// fakeEngine is the minimum core: no capability interfaces at all.
type fakeEngine struct{ engine.Engine }

func (fakeEngine) Capabilities() engine.Capabilities                    { return engine.Capabilities{} }
func (fakeEngine) Nodes(context.Context) ([]engine.Node, error)         { return nil, nil }
func (fakeEngine) Leader(context.Context) (string, error)               { return "n", nil }
func (fakeEngine) Workloads(context.Context) ([]engine.Workload, error) { return nil, nil }
func (fakeEngine) Restart(context.Context, string) error                { return nil }

// scalerEngine adds native scaling.
type scalerEngine struct {
	fakeEngine
	got int
}

func (s *scalerEngine) Scale(_ context.Context, _ string, replicas int) error {
	s.got = replicas
	return nil
}

// TestScaleUsesNativeWhenPresent: an engine that can scale is called directly,
// no composed path, no probing.
func TestScaleUsesNativeWhenPresent(t *testing.T) {
	eng := &scalerEngine{}
	if err := Scale(context.Background(), eng, "app_web", 3); err != nil {
		t.Fatalf("Scale: %v", err)
	}
	if eng.got != 3 {
		t.Fatalf("native Scale got %d replicas, want 3", eng.got)
	}
}

// TestScaleRefusesWhenTheEngineCannot: replicas are the engine's own primitive,
// so there is no honest composition — refusing beats a silent no-op that looks
// like a successful deploy.
func TestScaleRefusesWhenTheEngineCannot(t *testing.T) {
	err := Scale(context.Background(), fakeEngine{}, "app_web", 3)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

// TestCanaryNeedsNativeOrRouter: with neither a native rollout nor traffic
// routing there is no way to steer a fraction of requests, so "canary" cannot
// be composed honestly.
func TestCanaryNeedsNativeOrRouter(t *testing.T) {
	if _, err := Canary(context.Background(), fakeEngine{}, "app_web", "img:v2", 10); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

// lyingEngine declares capabilities it does not implement — the exact failure a
// boolean flag allows and a type assertion does not.
type lyingEngine struct{ fakeEngine }

func (lyingEngine) Capabilities() engine.Capabilities {
	return engine.Capabilities{NativeScale: true, NativeCanary: true}
}

// TestCoherentCatchesLyingAdapters: Coherent is the guard that keeps the
// declaration and the implementation from drifting apart.
func TestCoherentCatchesLyingAdapters(t *testing.T) {
	err := Coherent(lyingEngine{})
	if err == nil {
		t.Fatal("Coherent accepted an engine declaring capabilities it does not implement")
	}
	// The honest engine must pass.
	if err := Coherent(fakeEngine{}); err != nil {
		t.Fatalf("Coherent rejected an honest engine: %v", err)
	}
	if err := Coherent(&scalerEngine{}); err != nil {
		t.Fatalf("Coherent rejected a scaler that forgot to declare it: %v", err)
	}
}
