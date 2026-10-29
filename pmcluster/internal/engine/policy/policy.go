// Package policy is where the platform composes engine behaviour the engine
// does not provide natively.
//
// The split it enforces:
//
//	engine adapter  → does what its engine can do, nothing more
//	engine/policy   → provides the behaviour the PLATFORM promises, using
//	                  native capability when present and composing smaller
//	                  engine calls when it is not
//
// This is the difference between "Swarm has no canary" (true, and not a reason
// to have no canary) and "pmcluster has canary because the platform composes it
// from two workloads plus traffic weights".
package policy

import (
	"context"
	"errors"
	"fmt"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/engine"
)

// ErrUnsupported reports a promised behaviour that this engine cannot provide
// and that the platform cannot compose either.
//
// It is an error rather than a silent no-op: an operator asking to scale a
// workload must be told "this engine cannot" instead of finding out from a
// service that never grew.
var ErrUnsupported = errors.New("engine does not support this operation")

// Scale sets a workload's replica count.
//
// Native when the engine exposes Scaler. There is deliberately no fallback:
// replicas are the engine's own primitive, so an engine that cannot scale
// cannot have the count changed by composing other calls — the platform
// refuses instead of faking it.
func Scale(ctx context.Context, eng engine.Engine, workload string, replicas int) error {
	if replicas < 0 {
		return fmt.Errorf("scale %q: negative replica count %d", workload, replicas)
	}
	if s, ok := eng.(engine.Scaler); ok {
		return s.Scale(ctx, workload, replicas)
	}
	return fmt.Errorf("scale %q: %w: engine has no native scaling and replicas "+
		"cannot be composed from other calls", workload, ErrUnsupported)
}

// Canary runs a new version of a workload alongside the current one and shifts
// a fraction of traffic to it.
//
// Native when the engine provides CanaryDeployer. Otherwise it is composed at
// this layer from two primitives the platform already needs: launch a second
// workload running the new image, then move traffic with Router. Both are
// required — with a Router but no way to start a workload there is nothing to
// shift traffic to, and with CanaryDeployer absent and Router absent the
// platform refuses.
func Canary(ctx context.Context, eng engine.Engine, workload, image string, weight int) (string, error) {
	if weight < 0 || weight > 100 {
		return "", fmt.Errorf("canary %q: weight %d out of range 0-100", workload, weight)
	}
	if c, ok := eng.(engine.CanaryDeployer); ok {
		return c.StartCanary(ctx, workload, image)
	}
	if _, ok := eng.(engine.Router); !ok {
		return "", fmt.Errorf("canary %q: %w: engine has neither native canary "+
			"nor traffic routing, so no fraction of requests can be steered",
			workload, ErrUnsupported)
	}
	// Composed path: the platform launches the new version itself and then
	// moves traffic. The launch is a platform concern (see the manifest
	// renderer); this function's contract is that the weight is only moved
	// once that workload exists.
	return "", fmt.Errorf("canary %q: %w: composed rollouts are not implemented yet "+
		"(needs the platform-side workload launch that precedes the weight shift)",
		workload, ErrUnsupported)
}

// Coherent reports whether an engine's declared capabilities match the
// interfaces it actually implements.
//
// This is the guard against the one failure mode capability flags have: an
// adapter that sets a flag and never implements the method. Callers may assert
// on this in tests; production dispatch stays on type assertions, which cannot
// lie.
func Coherent(eng engine.Engine) error {
	c := eng.Capabilities()
	_, scaler := eng.(engine.Scaler)
	_, canary := eng.(engine.CanaryDeployer)
	_, autoscaler := eng.(engine.Autoscaler)
	_, jobs := eng.(engine.JobRunner)

	// One-directional on purpose: declaring a capability you do not implement
	// is the dangerous direction (the platform would skip work it must do
	// itself). Implementing one without declaring it is harmless — dispatch is
	// by type assertion, so the behaviour is simply there.
	var bad []string
	if c.NativeScale && !scaler {
		bad = append(bad, "NativeScale declared but Scaler not implemented")
	}
	if c.NativeCanary && !canary {
		bad = append(bad, "NativeCanary declared but CanaryDeployer not implemented")
	}
	if c.NativeAutoscale && !autoscaler {
		bad = append(bad, "NativeAutoscale declared but Autoscaler not implemented")
	}
	if c.NativeJobs && !jobs {
		bad = append(bad, "NativeJobs declared but JobRunner not implemented")
	}
	if len(bad) > 0 {
		return fmt.Errorf("engine %T declares capabilities it does not implement: %v", eng, bad)
	}
	return nil
}
