// Package reconcile implements the control-plane converge loop (L1): the
// daemon periodically re-derives desired state from the store and reports
// actual health, applying ONLY drift (never restarts). It is the counterpart
// to the badge surface — the loop writes stack_status snapshots to the DB and
// the badge endpoint reads them, so badges never touch the Docker daemon.
//
// Three passes per run:
//
//	a) Platform reconcile — re-render the platform configs and compare each
//	   stack's stored rendered_hash against the freshly rendered compose;
//	   redeploy drifted stacks (the same content-aware decision cluster
//	   update makes). Reuses cluster.Update's idempotent machinery (EnsureConfig
//	   reuses by hash, TLS secrets reuse by version) so running it on a
//	   converged cluster is a no-op.
//
//	b) App-stack drift — for every stack in the store, re-translate the latest
//	   source manifest and Sync when the rendered hash differs. Sync already
//	   no-ops on hash match, so a converged app stack is untouched.
//
//	c) Health snapshot — query the swarm for live service state and write a
//	   stack_status row per stack (plus per-service statuses) so the badge
//	   endpoint and the console read converged state from the DB.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/services"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// reconcileTracer is lazily created like the deploy/rollback tracers in
// internal/stacks: no-op until telemetry.Init wires the global provider.
var reconcileTracer trace.Tracer

func tracer() trace.Tracer {
	if reconcileTracer == nil {
		reconcileTracer = otel.Tracer("github.com/hazemarian/poor-man-cluster/pmcluster/internal/reconcile")
	}
	return reconcileTracer
}

// Status values shared with the badge surface (stack + service level).
const (
	StatusHealthy    = "healthy"
	StatusInProgress = "in progress"
	StatusDegraded   = "degraded"
	StatusError      = "error"
	StatusUnknown    = "unknown"
)

// completedRunOnce reports whether a one-shot job has finished: desired>0 but
// 0 running replicas. Its stale UpdateStatus (swarm leaves "paused" behind
// after a run-once task completes) must never drive error/degraded.
func completedRunOnce(s services.ServiceSummary) bool {
	return s.RunOnce && s.Desired > 0 && s.Replicas == 0
}

// ServiceStatus derives the badge status for one service, mirroring the
// derivation the badge endpoint used before it moved to DB snapshots.
func ServiceStatus(s services.ServiceSummary) string {
	switch {
	case s.UpdateState == "updating":
		return StatusInProgress
	case s.UpdateState == "paused" && !completedRunOnce(s):
		return StatusError
	case completedRunOnce(s):
		return StatusHealthy
	case s.Desired > 0 && s.Replicas < s.Desired:
		return StatusDegraded
	default:
		return StatusHealthy
	}
}

// StackStatus derives the aggregate status for a stack: an error outcome on
// the CURRENT revision wins, otherwise the worst live service status wins.
// A stack with no services reads unknown.
func StackStatus(st store.Stack, services []services.ServiceSummary) string {
	for _, e := range store.ParseStackErrors(st.LastError) {
		if e.Error != "" && e.Revision == st.CurrentRevision {
			return StatusError
		}
	}
	if len(services) == 0 {
		return StatusUnknown
	}
	worst := StatusHealthy
	for _, s := range services {
		st := ServiceStatus(s)
		if rank(st) > rank(worst) {
			worst = st
		}
	}
	return worst
}

// rank orders statuses so the aggregate picks the worst.
func rank(s string) int {
	switch s {
	case StatusError:
		return 4
	case StatusDegraded:
		return 3
	case StatusInProgress:
		return 2
	case StatusUnknown:
		return 1
	default:
		return 0
	}
}

// Reconciler is the daemon-side converge loop. Create it, wire the deps, then
// call RunOnce for a manual pass or Loop for the event-driven loop.
type Reconciler struct {
	Store         *store.Store
	Docker        runtime.Client
	DeployService *stacks.Service
	Services      services.Service
	Update        func(ctx context.Context, deps cluster.UpdateDeps, in cluster.UpdateInput) (*cluster.UpdateResult, error)
	UpdateDeps    cluster.UpdateDeps
	UpdateInput   cluster.UpdateInput
	Log           zerolog.Logger
	Interval      time.Duration // safety-net tick; 0 disables the tick

	mu    sync.Mutex
	runID int64
}

// RunOnce performs one converge pass. Safe to call concurrently (a second
// call while one is running returns nil immediately).
func (r *Reconciler) RunOnce(ctx context.Context) error {
	if !r.mu.TryLock() {
		r.Log.Debug().Msg("reconcile — pass skipped: another pass is in flight")
		return nil
	}
	defer r.mu.Unlock()
	id := atomic.AddInt64(&r.runID, 1)
	log := r.Log.With().Int64("run", id).Logger()

	// One root span per pass, with per-pass child spans for the three
	// sub-passes. With telemetry uninitialized this is a no-op provider.
	ctx, span := tracer().Start(ctx, "pmcluster.reconcile",
		trace.WithAttributes(attribute.Int64("run_id", id)))
	passErr := r.runPass(ctx, id, log)
	if passErr != nil {
		span.RecordError(passErr)
		span.SetStatus(codes.Error, passErr.Error())
	} else {
		span.SetStatus(codes.Ok, "converged")
	}
	span.End()

	log.Info().Msg("reconcile — pass completed")
	return passErr
}

// runPass executes the three reconcile passes; returns the first error (or nil
// when fully converged). Errors are recorded on the pass span but do not stop
// the remaining passes — a failing platform pass still gets health snapshots.
func (r *Reconciler) runPass(ctx context.Context, id int64, log zerolog.Logger) error {
	log.Info().Msg("reconcile — pass started")

	// (a) platform reconcile
	if r.Update != nil {
		_, platformSpan := tracer().Start(ctx, "pmcluster.reconcile.platform")
		log.Debug().Msg("reconcile — platform pass: re-rendering platform configs + comparing stored hashes")
		res, err := r.Update(ctx, r.UpdateDeps, r.UpdateInput)
		switch {
		case err != nil:
			log.Error().Err(err).Msg("reconcile — platform pass failed")
			platformSpan.RecordError(err)
			platformSpan.SetStatus(codes.Error, err.Error())
		case len(res.StacksDeployed) > 0:
			platformSpan.SetAttributes(attribute.StringSlice("stacks_redeployed", res.StacksDeployed))
			log.Info().Strs("stacks", res.StacksDeployed).Msg("reconcile — platform pass redeployed drifted stacks")
		default:
			log.Debug().Msg("reconcile — platform pass converged (no stack content changed)")
		}
		platformSpan.End()
	}

	// (b) app-stack drift. When a stack's storage node is down (offline or
	// drained), its sync is PAUSED: applying drift would recreate tasks on a
	// node that cannot serve the data, and the deploy itself could hang
	// waiting for the volume bind. The pause clears automatically when the
	// node returns to the swarm, or when a `stack move` re-pins the stack to
	// a healthy node (the pin changes, so the check passes again).
	stacks, err := r.Store.ListStacks(ctx)
	if err != nil {
		return fmt.Errorf("list stacks for reconcile: %w", err)
	}
	log.Debug().Int("stacks", len(stacks)).Msg("reconcile — app-stack drift pass: re-translating latest manifests")
	health := r.nodeHealth(ctx, log)
	for _, st := range stacks {
		if paused, node := r.storagePaused(ctx, st.Name, health); paused {
			log.Warn().Str("stack", st.Name).Str("node", node).Msg("reconcile — storage node down; pausing app sync (clears when the node returns or the stack is moved)")
			continue
		}
		res, serr := r.DeployService.Sync(ctx, st.Name)
		switch {
		case serr != nil:
			log.Error().Err(serr).Str("stack", st.Name).Msg("reconcile — app sync failed")
		case res != nil && res.Changed:
			log.Info().Str("stack", st.Name).Int64("revision", res.Revision).Msg("reconcile — app stack drifted, synced (new revision)")
		default:
			log.Debug().Str("stack", st.Name).Msg("reconcile — app stack converged (rendered hash unchanged, no-op)")
		}
	}

	// (c) health snapshot
	return r.snapshotHealth(ctx, log)
}

// nodeHealth maps each swarm node hostname to whether it is ready to host
// storage: present in the node list, Status "ready" and Availability
// "active". Returns nil when the swarm cannot be queried (standalone/local
// mode or a NodeList failure) — callers then skip the pause logic entirely
// and behave exactly as before.
func (r *Reconciler) nodeHealth(ctx context.Context, log zerolog.Logger) map[string]bool {
	if r.Docker == nil {
		return nil
	}
	nodes, err := r.Docker.NodeList(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("reconcile — node list failed; storage-outage pause disabled this pass")
		return nil
	}
	health := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		health[n.Hostname] = n.Status == "ready" && n.Availability == "active"
	}
	return health
}

// storagePaused reports whether the stack's storage node is currently down.
// A stack is never paused when the swarm is not queryable (nil health), when
// it has no storage pin (stateless, or role-based placement), or when the
// deploy service / resolver is not configured.
func (r *Reconciler) storagePaused(ctx context.Context, stackName string, health map[string]bool) (bool, string) {
	if r.DeployService == nil || len(health) == 0 {
		return false, ""
	}
	pins, err := r.DeployService.StoragePinsForStack(ctx, stackName)
	if err != nil {
		r.Log.Warn().Err(err).Str("stack", stackName).Msg("reconcile — storage-pin resolution failed; not pausing")
		return false, ""
	}
	for _, pin := range pins {
		if !health[pin] {
			return true, pin
		}
	}
	return false, ""
}

// snapshotHealth writes a stack_status row per store stack, with per-service
// statuses derived from the live swarm. A stack the swarm no longer knows
// reads unknown. Also clears status rows for stacks removed from the store.
func (r *Reconciler) snapshotHealth(ctx context.Context, log zerolog.Logger) error {
	stackRows, err := r.Store.ListStacks(ctx)
	if err != nil {
		return fmt.Errorf("list stacks for health snapshot: %w", err)
	}
	seen := map[string]bool{}
	for _, st := range stackRows {
		seen[st.Name] = true
		svcs, err := r.Services.List(ctx, st.Name)
		if err != nil {
			log.Error().Err(err).Str("stack", st.Name).Msg("reconcile — service list failed")
			if serr := r.Store.SetStackStatus(ctx, store.StackStatus{StackName: st.Name, Status: StatusUnknown, Services: map[string]string{}, UpdatedAt: time.Now().Unix()}); serr != nil {
				log.Error().Err(serr).Str("stack", st.Name).Msg("reconcile — store stack status failed")
			}
			continue
		}
		svcStatus := map[string]string{}
		for _, s := range svcs {
			svcStatus[s.Name] = ServiceStatus(s)
			log.Debug().Str("stack", st.Name).Str("service", s.Name).
				Str("status", ServiceStatus(s)).Uint64("replicas", s.Replicas).
				Uint64("desired", s.Desired).Str("update_state", s.UpdateState).
				Bool("run_once", s.RunOnce).Msg("reconcile — service status derived")
		}
		status := StackStatus(*st, svcs)
		log.Debug().Str("stack", st.Name).Str("status", status).Int("services", len(svcs)).
			Msg("reconcile — stack status derived")
		if err := r.Store.SetStackStatus(ctx, store.StackStatus{StackName: st.Name, Status: status, Services: svcStatus, UpdatedAt: time.Now().Unix()}); err != nil {
			log.Error().Err(err).Str("stack", st.Name).Msg("reconcile — store stack status failed")
		}
	}
	// Tidy: remove snapshot rows for stacks no longer in the store.
	rows, err := r.Store.ListStackStatuses(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if !seen[row.StackName] {
			// Best-effort cleanup; SetStackStatus would re-add, so use the
			// store's delete path.
			if err := r.Store.DeleteStackStatus(ctx, row.StackName); err != nil && !errors.Is(err, store.ErrNotFound) {
				log.Error().Err(err).Str("stack", row.StackName).Msg("reconcile — prune stale status failed")
			} else {
				log.Debug().Str("stack", row.StackName).Msg("reconcile — pruned stale stack status row")
			}
		}
	}
	log.Debug().Int("stacks", len(seen)).Int("status_rows", len(rows)).Msg("reconcile — health snapshot written")
	return nil
}

// Loop runs reconcile passes until ctx is cancelled. It is event-driven when
// Docker events are available (each swarm event triggers a debounced pass) and
// falls back to the safety-net ticker (Interval). Interval 0 (the default)
// disables the loop entirely — daemons opt into convergence with
// reconcile_interval > 0.
func (r *Reconciler) Loop(ctx context.Context) {
	log := r.Log.With().Str("component", "reconcile").Logger()
	if r.Interval <= 0 {
		log.Info().Msg("reconcile loop — disabled (reconcile_interval 0)")
		return
	}
	var evCh <-chan runtime.Event
	if r.Docker != nil {
		evCh, _ = r.Docker.Events(ctx, time.Now().Add(-time.Minute))
		log.Debug().Msg("reconcile loop — watching Docker swarm events")
	}
	var ticker *time.Ticker
	if r.Interval > 0 {
		ticker = time.NewTicker(r.Interval)
		defer ticker.Stop()
		log.Debug().Dur("interval", r.Interval).Msg("reconcile loop — safety-net ticker armed")
	}
	// Debounce: coalesce bursts of events into one pass.
	var trigger <-chan time.Time
	var timer *time.Timer
	const debounce = 2 * time.Second
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("reconcile loop stopped")
			return
		case ev := <-evCh:
			log.Debug().Str("type", ev.Type).Str("action", ev.Action).Str("stack", ev.Stack).
				Str("service", ev.Service).Msg("reconcile loop — swarm event received")
			if timer == nil {
				log.Debug().Dur("debounce", debounce).Msg("reconcile loop — debounce timer armed")
				timer = time.NewTimer(debounce)
				trigger = timer.C
			}
		case <-trigger:
			timer.Stop()
			timer = nil
			trigger = nil
			log.Debug().Msg("reconcile loop — debounce elapsed, running pass")
			r.pass(ctx, log)
		case <-tickIf(ticker, ctx):
			log.Debug().Msg("reconcile loop — safety-net tick, running pass")
			r.pass(ctx, log)
		}
	}
}

// pass runs one guarded pass with error capture for the /health surface.
func (r *Reconciler) pass(ctx context.Context, log zerolog.Logger) {
	if err := r.RunOnce(ctx); err != nil {
		log.Error().Err(err).Msg("reconcile pass failed")
	}
}

// tickIf adapts a nil-safe ticker channel for the select.
func tickIf(t *time.Ticker, ctx context.Context) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}
