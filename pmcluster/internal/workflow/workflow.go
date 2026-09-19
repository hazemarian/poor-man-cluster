// Package workflow runs named steps in order with visible progress. It is
// the shared engine behind cluster up, cluster update and the deploy
// pipeline.
package workflow

import (
	"context"
	"fmt"
	"io"
)

// Step is a named unit of work in a workflow. Steps run in order and the
// first failure aborts the run with the step name attached to the error.
type Step struct {
	Name string
	Run  func(ctx context.Context) error
}

// Workflow runs named steps in order, streaming a "▶ <name>" progress line
// per step to its output. It is the shared engine behind `cluster up`,
// `cluster update` and the deploy pipeline: each phase of those operations
// is one step, so progress is visible, cancellation is honoured via ctx, and
// failures point at the exact phase that went wrong.
type Workflow struct {
	out   io.Writer
	steps []Step
}

// NewWorkflow returns a workflow that streams progress to out (nil = discard).
func NewWorkflow(out io.Writer) *Workflow {
	if out == nil {
		out = io.Discard
	}
	return &Workflow{out: out}
}

// Add appends a named step to the workflow.
func (w *Workflow) Add(name string, run func(ctx context.Context) error) *Workflow {
	w.steps = append(w.steps, Step{Name: name, Run: run})
	return w
}

// Steps returns the ordered step names added so far. It is a snapshot — later
// Add calls are not reflected in a previously returned slice.
func (w *Workflow) Steps() []string {
	out := make([]string, 0, len(w.steps))
	for _, s := range w.steps {
		out = append(out, s.Name)
	}
	return out
}

// Run executes every step in order. Each step's name is printed as a
// "▶ <name>" progress line before it runs. The first error stops the run
// and is wrapped with the failing step's name. Run is safe to call once.
func (w *Workflow) Run(ctx context.Context) error {
	for _, s := range w.steps {
		fmt.Fprintf(w.out, "▶ %s\n", s.Name)
		if err := s.Run(ctx); err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
	}
	return nil
}
