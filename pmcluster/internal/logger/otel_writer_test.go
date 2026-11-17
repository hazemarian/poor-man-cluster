package logger

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// captureProvider records emitted log records so a test can assert what actually left the process.
type captureProvider struct {
	records []sdklog.Record
}

type captureExporter struct{ p *captureProvider }

func (e captureExporter) Export(_ context.Context, recs []sdklog.Record) error {
	for _, r := range recs {
		e.p.records = append(e.p.records, r.Clone())
	}
	return nil
}
func (e captureExporter) Shutdown(context.Context) error   { return nil }
func (e captureExporter) ForceFlush(context.Context) error { return nil }

func withCapture(t *testing.T) *captureProvider {
	t.Helper()
	p := &captureProvider{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(captureExporter{p: p})))
	prev := global.GetLoggerProvider()
	global.SetLoggerProvider(lp)
	t.Cleanup(func() {
		_ = lp.Shutdown(context.Background())
		global.SetLoggerProvider(prev)
	})
	return p
}

// TestOTelWriter_NeverEmitsAnEmptyBody is the regression test for a shared-stream outage: Parseable
// rejects a record whose body carries no known value with
//
//	Could not deserialize into JSON object, Invalid data for Value, no known keys found
//
// and rejects the WHOLE BATCH it arrived in. The edge began shipping its logs and every service's
// logs went dark, because one kind of zerolog event has no `message` field. Measured against the
// backend: a record with an empty body answers 400, the same record with any body answers 200.
func TestOTelWriter_NeverEmitsAnEmptyBody(t *testing.T) {
	p := withCapture(t)
	w := newOTelWriter()

	// A zerolog event with fields and NO message — the shape that broke the stream.
	line := `{"level":"info","time":"2026-11-17T09:35:00Z","stack":"aarma"}` + "\n"
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(p.records) != 1 {
		t.Fatalf("records emitted = %d, want 1", len(p.records))
	}
	body := p.records[0].Body()
	if body.Kind() != log.KindString || strings.TrimSpace(body.AsString()) == "" {
		t.Fatalf("body = %v (%v), want a non-empty string", body.AsString(), body.Kind())
	}
	// The fallback must be the original event, so the line is still diagnosable.
	if !strings.Contains(body.AsString(), `"stack":"aarma"`) {
		t.Errorf("fallback body = %q, want the raw event", body.AsString())
	}
}

// TestOTelWriter_KeepsTheMessageWhenThere is one — the ordinary path.
func TestOTelWriter_KeepsTheMessageWhenThereIsOne(t *testing.T) {
	p := withCapture(t)
	w := newOTelWriter()
	line := `{"level":"warn","time":"2026-11-17T09:35:00Z","message":"terminal: refused a socket"}` + "\n"
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(p.records) != 1 {
		t.Fatalf("records emitted = %d, want 1", len(p.records))
	}
	if got := p.records[0].Body().AsString(); got != "terminal: refused a socket" {
		t.Errorf("body = %q, want the message", got)
	}
	if sev := p.records[0].Severity(); sev != log.SeverityWarn {
		t.Errorf("severity = %v, want warn", sev)
	}
}

// TestOTelWriter_DropsUnparsableInputWithoutEmitting keeps the existing contract: half a JSON line is
// not a record, and inventing one would poison a batch too.
func TestOTelWriter_DropsUnparsableInputWithoutEmitting(t *testing.T) {
	p := withCapture(t)
	w := newOTelWriter()
	if _, err := w.Write([]byte("not json at all\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(p.records) != 0 {
		t.Fatalf("records emitted = %d, want 0", len(p.records))
	}
}

// TestOTelWriter_ForwardsStructuredFields keeps the fields queryable in the backend.
func TestOTelWriter_ForwardsStructuredFields(t *testing.T) {
	p := withCapture(t)
	w := newOTelWriter()
	line, _ := json.Marshal(map[string]any{
		"level": "error", "message": "deploy failed", "stack": "aarma", "attempt": 2,
	})
	if _, err := w.Write(append(line, '\n')); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(p.records) != 1 {
		t.Fatalf("records emitted = %d, want 1", len(p.records))
	}
	// sdklog.Record iterates its attributes; there is no Attributes() slice accessor.
	attrs := map[string]string{}
	p.records[0].WalkAttributes(func(a log.KeyValue) bool {
		attrs[a.Key] = a.Value.AsString()
		return true
	})
	if attrs["stack"] != "aarma" {
		t.Errorf("stack attribute = %q, want aarma", attrs["stack"])
	}
	if _, ok := attrs["level"]; ok {
		t.Error("level leaked into attributes; it belongs in severity")
	}
}
