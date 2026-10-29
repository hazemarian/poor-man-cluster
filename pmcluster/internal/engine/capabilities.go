package engine

import "context"

// Capability interfaces.
//
// The core Engine interface is what every engine must do. These are optional:
// an adapter implements the ones its engine can do natively, and the platform
// layer (internal/engine/policy) composes the rest out of smaller calls.
//
// Dispatch is by TYPE ASSERTION, never by a capability flag. A flag can be set
// by an adapter that implements nothing — a wrong boolean compiles happily and
// fails at 3am. A missing method cannot: `if s, ok := eng.(Scaler); ok` is the
// truth, and Capabilities() is only a declaration the coherence test keeps
// honest.
//
//	eng, _ := ...
//	if s, ok := eng.(engine.Scaler); ok { s.Scale(ctx, w, n) }   // native
//	else                                { policy.Scale(...) }    // composed

// Scaler sets a workload's replica count natively.
//
// An engine with no scale concept at all cannot be emulated: replicas are the
// engine's own primitive. If an engine can run a workload it can almost
// certainly scale it, so this is expected to be near-universal — its absence
// is a signal that the platform must refuse, not invent.
type Scaler interface {
	Scale(ctx context.Context, workload string, replicas int) error
}

// CanaryDeployer runs a new version alongside the current one and can promote
// or abort it.
//
// Engines that lack this (Swarm, k8s without a rollout controller) get the
// behaviour composed at the platform layer: two workloads plus traffic
// weighting. That composition needs a Router, so canary emulation is only
// attempted when the engine exposes one.
type CanaryDeployer interface {
	// StartCanary launches image alongside the workload's current version and
	// returns a handle for Promote/Abort.
	StartCanary(ctx context.Context, workload, image string) (rolloutID string, err error)
	Promote(ctx context.Context, rolloutID string) error
	Abort(ctx context.Context, rolloutID string) error
}

// Router shifts traffic between the versions of a workload.
//
// This is the primitive canary emulation is built from: without a way to send
// a fraction of requests to the new workload, "canary" would mean "run it and
// hope nobody notices", which is not a rollout strategy.
type Router interface {
	// SetWeight sends weight percent of a workload's traffic to target, where
	// target is a workload name. Weight 0 restores all traffic to the primary.
	SetWeight(ctx context.Context, workload, target string, weight int) error
}

// Autoscaler adjusts a workload's replicas from a metric reading.
//
// Engines with this native ship the whole control loop, hysteresis included;
// for the rest, the platform owns the loop and calls Scaler — which is why
// autoscale emulation requires Scaler but not Autoscaler.
type Autoscaler interface {
	// Autoscale applies one control decision. The platform's loop supplies the
	// target, having done the metrics work itself.
	Autoscale(ctx context.Context, workload string, replicas int) error
}

// JobRunner launches a run-once workload and reports its completion.
//
// The absence of this is the one gap with no clean composition on Swarm:
// `--mode replicated-job` exists, but nothing reaps finished jobs or reports
// their history, so the platform ends up owning both.
type JobRunner interface {
	RunJob(ctx context.Context, spec Workload) (jobID string, err error)
	JobStatus(ctx context.Context, jobID string) (JobState, error)
}

// JobState is the lifecycle of a run-once workload. A job that ran and exited
// successfully and a job that never started both look like "zero running" to
// an engine — the difference is a platform concern, which is why it is modelled.
type JobState struct {
	ID       string
	Running  bool
	Attempts int
	// Done reports the job reached a terminal state (success or failure).
	Done bool
	// Succeeded is meaningful only when Done is true.
	Succeeded bool
	Reason    string
}
