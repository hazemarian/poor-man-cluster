package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/services"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// fakeBadgeSvc is a minimal services.Service stub: embeds the interface so the
// unimplemented methods panic if ever called, and returns a canned List.
type fakeBadgeSvc struct {
	services.Service
	svcs []services.ServiceSummary
	err  error
}

func (f *fakeBadgeSvc) List(_ context.Context, _ string) ([]services.ServiceSummary, error) {
	return f.svcs, f.err
}

func TestStackBadge_StatusDerivation(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/data.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	// Seed a stack with a revision so CurrentRevision is set.
	if err := st.RecordDeploy(ctx, &store.StackRevision{StackName: "demo", Revision: 1002, SourceYAML: "app: demo", RenderedYAML: "x: 1"}, ""); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}

	svc := &fakeBadgeSvc{svcs: []services.ServiceSummary{
		{Name: "demo_web", Desired: 1, Replicas: 1},
	}}

	cases := []struct {
		name  string
		setup func()
		svcs  []services.ServiceSummary
		want  string
	}{
		{"deployed", func() {}, svc.svcs, "deployed"},
		{"error on current revision", func() {
			_, _ = st.RecordStackError(ctx, "demo", 1002, "docker stack deploy: boom")
		}, svc.svcs, "error"},
		{"stale error ignored (older revision)", func() {
			// fresh stack: error on a non-current revision
		}, svc.svcs, "deployed"},
		{"in progress", func() {}, []services.ServiceSummary{
			{Name: "demo_web", Desired: 1, Replicas: 1, UpdateState: "updating"},
		}, "in progress"},
		{"paused is error", func() {}, []services.ServiceSummary{
			{Name: "demo_web", Desired: 1, Replicas: 1, UpdateState: "paused"},
		}, "error"},
		{"degraded under-replicated", func() {}, []services.ServiceSummary{
			{Name: "demo_web", Desired: 2, Replicas: 1},
		}, "degraded"},
		{"completed run-once not degraded", func() {}, []services.ServiceSummary{
			{Name: "demo_migrate", Desired: 1, Replicas: 0, RunOnce: true},
		}, "deployed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Reset the stack error history for each case.
			_ = st.SetStackLastError(ctx, "demo", "")
			tc.setup()
			svc.svcs = tc.svcs
			_, status := stackBadge(ctx, st, svc, "demo")
			if status != tc.want {
				t.Errorf("status = %q, want %q", status, tc.want)
			}
		})
	}

	// Unknown stack → unknown.
	_, status := stackBadge(ctx, st, svc, "ghost")
	if status != "unknown" {
		t.Errorf("ghost status = %q, want unknown", status)
	}
}

func TestBadgeHTTP_ReturnsSVG(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/data.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	_ = st.RecordDeploy(ctx, &store.StackRevision{StackName: "demo", Revision: 1002, SourceYAML: "app: demo", RenderedYAML: "x: 1"}, "")

	svc := &fakeBadgeSvc{svcs: []services.ServiceSummary{
		{Name: "demo_web", Desired: 1, Replicas: 1},
	}}

	r := chi.NewRouter()
	BadgeMount(r, st, svc)

	req := httptest.NewRequest(http.MethodGet, "/api/public/badge/demo", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("Content-Type = %q, want image/svg+xml", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<svg") || !strings.Contains(body, "deployed") {
		t.Errorf("body missing SVG or status:\n%s", body)
	}
	if !strings.Contains(body, "#44d47b") {
		t.Errorf("body missing deployed color:\n%s", body)
	}

	// Unknown stack still returns a valid SVG.
	req2 := httptest.NewRequest(http.MethodGet, "/api/public/badge/ghost", nil)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if !strings.Contains(rec2.Body.String(), "unknown") {
		t.Errorf("ghost body missing unknown status:\n%s", rec2.Body.String())
	}
}

func TestEscapeXML(t *testing.T) {
	in := `<demo & "quoted" 'single'>`
	out := escapeXML(in)
	for _, bad := range []string{"<", ">", `"`, "'"} {
		if strings.Contains(out, bad) {
			t.Errorf("escapeXML(%q) still contains %q: %q", in, bad, out)
		}
	}
	if !strings.Contains(out, "&lt;") || !strings.Contains(out, "&amp;") || !strings.Contains(out, "&quot;") {
		t.Errorf("escapeXML(%q) missing entities: %q", in, out)
	}
}
