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
		{"completed run-once paused not error", func() {}, []services.ServiceSummary{
			{Name: "demo_migrate", Desired: 1, Replicas: 0, RunOnce: true, UpdateState: "paused"},
		}, "deployed"},
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

	// Service-level badge route.
	req3 := httptest.NewRequest(http.MethodGet, "/api/public/badge/demo/demo_web", nil)
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, req3)
	if !strings.Contains(rec3.Body.String(), "demo/demo_web: deployed") {
		t.Errorf("service badge body missing label/status:\n%s", rec3.Body.String())
	}
	req4 := httptest.NewRequest(http.MethodGet, "/api/public/badge/demo/ghost_svc", nil)
	rec4 := httptest.NewRecorder()
	r.ServeHTTP(rec4, req4)
	if !strings.Contains(rec4.Body.String(), "demo/ghost_svc: unknown") {
		t.Errorf("ghost service badge body missing unknown status:\n%s", rec4.Body.String())
	}

	// Combined multi-segment badge: main health + one segment per service.
	req5 := httptest.NewRequest(http.MethodGet, "/api/public/badge/demo/services", nil)
	rec5 := httptest.NewRecorder()
	r.ServeHTTP(rec5, req5)
	if rec5.Code != http.StatusOK {
		t.Fatalf("services badge status = %d, want 200", rec5.Code)
	}
	body5 := rec5.Body.String()
	if !strings.Contains(body5, "demo: deployed") || !strings.Contains(body5, "web: deployed") {
		t.Errorf("combined badge missing main + service segments:\n%s", body5)
	}
	if !strings.Contains(body5, "·") {
		t.Errorf("combined badge missing segment separator:\n%s", body5)
	}
}

func TestServiceBadge_StatusDerivation(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/data.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	_ = st.RecordDeploy(ctx, &store.StackRevision{StackName: "demo", Revision: 1002, SourceYAML: "app: demo", RenderedYAML: "x: 1"}, "")

	cases := []struct {
		name string
		svc  services.ServiceSummary
		want string
	}{
		{"deployed", services.ServiceSummary{Name: "demo_web", Desired: 1, Replicas: 1}, "deployed"},
		{"in progress", services.ServiceSummary{Name: "demo_web", Desired: 1, Replicas: 1, UpdateState: "updating"}, "in progress"},
		{"paused is error", services.ServiceSummary{Name: "demo_web", Desired: 1, Replicas: 1, UpdateState: "paused"}, "error"},
		{"completed run-once paused is deployed", services.ServiceSummary{Name: "demo_migrate", Desired: 1, Replicas: 0, RunOnce: true, UpdateState: "paused"}, "deployed"},
		{"degraded", services.ServiceSummary{Name: "demo_web", Desired: 2, Replicas: 1}, "degraded"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeBadgeSvc{svcs: []services.ServiceSummary{tc.svc}}
			_, status := serviceBadge(ctx, st, svc, "demo", tc.svc.Name)
			if status != tc.want {
				t.Errorf("status = %q, want %q", status, tc.want)
			}
		})
	}

	// Unknown service + unknown stack.
	svc := &fakeBadgeSvc{svcs: []services.ServiceSummary{{Name: "demo_web", Desired: 1, Replicas: 1}}}
	if _, status := serviceBadge(ctx, st, svc, "demo", "ghost"); status != "unknown" {
		t.Errorf("ghost service status = %q, want unknown", status)
	}
	if _, status := serviceBadge(ctx, st, svc, "ghost", "demo_web"); status != "unknown" {
		t.Errorf("ghost stack status = %q, want unknown", status)
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
