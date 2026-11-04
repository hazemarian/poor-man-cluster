package backupagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/otel"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// A run must be traceable phase by phase. The production incident this exists
// for was a seventeen-minute freeze reported as one number — "wrote blocked for
// 17m9s" — which cannot say whether fsfreeze or lvcreate held the window.
func TestRunEmitsAPhaseByPhaseTrace(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(prev)

	root, archive := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "index.html"), []byte("<h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &Agent{
		Node:       "node-1",
		VolumeRoot: root,
		ArchiveDir: archive,
		Snapshot:   &NoSnapshot{Root: root},
		Retention:  &backups.Retention{KeepHours: 48, KeepDays: 15, KeepLast: 2},
	}
	if _, err := a.Run(context.Background(), Plan{Node: "node-1", Stacks: []StackPlan{{Name: "web"}}}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	names := map[string]bool{}
	for _, s := range exp.GetSpans() {
		names[s.Name] = true
	}
	for _, want := range []string{"backup.run", "backup.stack", "backup.retention"} {
		if !names[want] {
			t.Fatalf("missing span %q; got %v", want, names)
		}
	}
}
