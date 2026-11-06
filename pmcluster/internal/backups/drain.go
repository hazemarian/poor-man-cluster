package backups

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// Scaler is the slice of the container runtime a restore needs to pause a
// stack. It is a narrow interface rather than a widening of runtime.Client so
// that a fake without a copy of every runtime method can still drive a restore
// (and so the drain can be tested without Docker at all).
type Scaler interface {
	ServiceList(ctx context.Context) ([]runtime.Service, error)
	ScaleService(ctx context.Context, serviceID string, replicas uint64) error
}

// DefaultDrainWait bounds how long Drain waits for tasks to stop.
const DefaultDrainWait = 2 * time.Minute

// Drainer pauses a stack's services around a restore and puts them back.
type Drainer struct {
	Scaler Scaler
	// Logf reports what was paused. Optional.
	Logf func(format string, args ...any)
	// Wait overrides DefaultDrainWait.
	Wait time.Duration
}

// DrainReport says what a drain did, so the caller can tell the operator.
type DrainReport struct {
	Stack   string
	Paused  map[string]uint64 // service name -> replica count to restore
	Skipped []string          // global / one-shot services that were left running
}

// Names returns the paused service names in a stable order.
func (r DrainReport) Names() []string {
	out := make([]string, 0, len(r.Paused))
	for name := range r.Paused {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Drain scales every replicated service of `stack` to zero and waits until none
// of them has a running task, returning the means to put them back.
//
// Why a restore needs this: it writes into the live volume and then removes
// whatever the snapshot did not hold (pruneToSnapshot). A running database keeps
// writing into files that are being unlinked underneath it, and what is left when
// it is finally started is a directory built out of two different moments — the
// classic "could not locate required checkpoint record", or a database that opens
// cleanly and has lost an hour of committed transactions.
//
// Global services cannot be paused without removing and recreating them, so they
// are reported in Skipped and left running: an operator restoring a stack that
// has one gets a warning that names it, not a silent lie about a drained stack.
// One-shot jobs are ignored — a finished task is not a writer.
//
// The returned undo is safe to call after a failed restore and safe to call
// twice; it never uses the restore's context (which may already be cancelled).
func (d *Drainer) Drain(ctx context.Context, stack string) (DrainReport, func(), error) {
	rep := DrainReport{Stack: stack, Paused: map[string]uint64{}}
	noop := func() {}
	if d == nil || d.Scaler == nil || stack == "" {
		return rep, noop, nil
	}

	svcs, err := d.Scaler.ServiceList(ctx)
	if err != nil {
		return rep, noop, fmt.Errorf("drain %s: list services: %w", stack, err)
	}
	var paused []runtime.Service
	for _, s := range svcs {
		if s.Stack != stack {
			continue
		}
		switch {
		case s.Mode == "global":
			rep.Skipped = append(rep.Skipped, s.Name)
			continue
		case s.RunOnce:
			continue
		}
		// Desired is the spec's replica count; Replicas is what is running. A
		// service scaled to 0 by someone else must not be scaled back UP by a
		// restore that found it down.
		want := s.Desired
		if want == 0 {
			continue
		}
		rep.Paused[s.Name] = want
		paused = append(paused, s)
	}
	if len(paused) == 0 {
		return rep, noop, nil // nothing of this stack is running here
	}

	undo := func() {
		bg := context.WithoutCancel(ctx)
		for _, s := range paused {
			n := rep.Paused[s.Name]
			if err := d.Scaler.ScaleService(bg, s.ID, n); err != nil && d.Logf != nil {
				d.Logf("warning: could not scale %s back to %d: %v", s.Name, n, err)
			}
		}
	}

	for _, s := range paused {
		if err := d.Scaler.ScaleService(ctx, s.ID, 0); err != nil {
			undo()
			return rep, noop, fmt.Errorf("drain %s: scale %s to 0: %w", stack, s.Name, err)
		}
	}
	if d.Logf != nil {
		d.Logf("paused %d service(s) of %s: %s", len(paused), stack, strings.Join(rep.Names(), ", "))
	}

	wait := d.Wait
	if wait <= 0 {
		wait = DefaultDrainWait
	}
	deadline := time.Now().Add(wait)
	for {
		running, rerr := d.stillRunning(ctx, stack, rep.Paused)
		if rerr != nil {
			undo()
			return rep, noop, fmt.Errorf("drain %s: %w", stack, rerr)
		}
		if len(running) == 0 {
			return rep, undo, nil
		}
		if time.Now().After(deadline) {
			undo()
			return rep, noop, fmt.Errorf(
				"drain %s: still running after %s (%s) — refusing to restore under a live database; "+
					"re-run with --drain=false if the stack is already down or you accept the risk",
				stack, wait, strings.Join(running, ", "))
		}
		select {
		case <-ctx.Done():
			undo()
			return rep, noop, ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}
	}
}

// stillRunning names the paused services that still report a running task.
func (d *Drainer) stillRunning(ctx context.Context, stack string, paused map[string]uint64) ([]string, error) {
	svcs, err := d.Scaler.ServiceList(ctx)
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	var out []string
	for _, s := range svcs {
		if s.Stack != stack {
			continue
		}
		if _, ok := paused[s.Name]; !ok {
			continue
		}
		if s.Replicas > 0 {
			out = append(out, s.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// RestoreIntoLiveVolume reports whether a restore target is the live volume root
// (or inside it) — the only case where pausing the stack buys anything.
//
// Restoring an artifact into a scratch directory to look inside it must never
// scale a production app to zero, and "the flag defaults to on" makes that the
// easy mistake to make.
func RestoreIntoLiveVolume(destRoot, volumeRoot string) bool {
	if destRoot == "" || volumeRoot == "" {
		return false
	}
	d := filepath.Clean(destRoot)
	v := filepath.Clean(volumeRoot)
	if d == v {
		return true
	}
	return strings.HasPrefix(d, v+string(filepath.Separator))
}
