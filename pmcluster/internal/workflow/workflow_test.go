package workflow

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestWorkflowRunsStepsInOrder(t *testing.T) {
	var buf bytes.Buffer
	w := NewWorkflow(&buf)
	var order []string
	w.Add("one", func(context.Context) error { order = append(order, "one"); return nil })
	w.Add("two", func(context.Context) error { order = append(order, "two"); return nil })

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(order) != 2 || order[0] != "one" || order[1] != "two" {
		t.Errorf("steps ran out of order: %v", order)
	}
	if got := buf.String(); !strings.Contains(got, "▶ one\n") || !strings.Contains(got, "▶ two\n") {
		t.Errorf("progress lines missing: %q", got)
	}
}

func TestWorkflowStopsOnFirstError(t *testing.T) {
	var buf bytes.Buffer
	w := NewWorkflow(&buf)
	w.Add("ok", func(context.Context) error { return nil })
	w.Add("boom", func(context.Context) error { return errors.New("kaboom") })
	w.Add("never", func(context.Context) error { t.Error("step ran after failure"); return nil })

	err := w.Run(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "boom: kaboom") {
		t.Errorf("error should carry step name, got %q", err)
	}
	// Progress is printed before the step runs, so "▶ never" appears, but
	// its Run closure must not execute (the t.Error in the closure would fail).
}

func TestWorkflowCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w := NewWorkflow(nil)
	w.Add("wait", func(ctx context.Context) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	})
	if err := w.Run(ctx); err == nil {
		t.Fatal("expected context cancellation error")
	}
}

func TestWorkflowNilOutput(t *testing.T) {
	w := NewWorkflow(nil) // must not panic
	w.Add("x", func(context.Context) error { return nil })
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestWorkflowStepsSnapshot(t *testing.T) {
	w := NewWorkflow(nil)
	w.Add("a", func(context.Context) error { return nil })
	snap := w.Steps()
	if len(snap) != 1 || snap[0] != "a" {
		t.Fatalf("snapshot wrong: %v", snap)
	}
	w.Add("b", func(context.Context) error { return nil })
	if len(snap) != 1 {
		t.Errorf("snapshot should be immutable, got %d steps", len(snap))
	}
	if got := w.Steps(); len(got) != 2 {
		t.Errorf("fresh Steps should reflect new adds, got %v", got)
	}
}

func TestWorkflowEmpty(t *testing.T) {
	if err := NewWorkflow(nil).Run(context.Background()); err != nil {
		t.Fatalf("empty workflow: %v", err)
	}
}
