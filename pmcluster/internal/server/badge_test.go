package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// badgeStore opens a fresh store with a seeded stack_status snapshot.
func badgeStore(t *testing.T, status string, services map[string]string) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/data.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if status != "" {
		if err := st.SetStackStatus(context.Background(), store.StackStatus{
			StackName: "demo", Status: status, Services: services, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("SetStackStatus: %v", err)
		}
	}
	return st
}

func TestStackBadge_ReadsDBSnapshot(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name   string
		status string
		want   string
	}{
		{"healthy", "healthy", "healthy"},
		{"in progress", "in progress", "in progress"},
		{"degraded", "degraded", "degraded"},
		{"error", "error", "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := badgeStore(t, tc.status, nil)
			_, status := stackBadge(ctx, st, "demo")
			if status != tc.want {
				t.Errorf("status = %q, want %q", status, tc.want)
			}
		})
	}

	// No snapshot row → unknown (the loop has not written yet).
	st := badgeStore(t, "", nil)
	if _, status := stackBadge(ctx, st, "demo"); status != "unknown" {
		t.Errorf("no-snapshot status = %q, want unknown", status)
	}
	if _, status := stackBadge(ctx, st, "ghost"); status != "unknown" {
		t.Errorf("ghost status = %q, want unknown", status)
	}
}

func TestServiceBadge_ReadsDBSnapshot(t *testing.T) {
	ctx := context.Background()
	st := badgeStore(t, "healthy", map[string]string{"web": "healthy", "migrate": "healthy"})

	if _, status := serviceBadge(ctx, st, "demo", "web"); status != "healthy" {
		t.Errorf("web status = %q, want healthy", status)
	}
	if _, status := serviceBadge(ctx, st, "demo", "ghost_svc"); status != "unknown" {
		t.Errorf("ghost service status = %q, want unknown", status)
	}
	if _, status := serviceBadge(ctx, st, "ghost", "web"); status != "unknown" {
		t.Errorf("ghost stack status = %q, want unknown", status)
	}
}

func TestBadgeHTTP_ReturnsSVG(t *testing.T) {
	st := badgeStore(t, "healthy", map[string]string{"web": "healthy"})
	r := chi.NewRouter()
	BadgeMount(r, st)

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
	if !strings.Contains(body, "<svg") || !strings.Contains(body, "healthy") {
		t.Errorf("body missing SVG or status:\n%s", body)
	}
	if !strings.Contains(body, "#44d47b") {
		t.Errorf("body missing healthy color:\n%s", body)
	}

	// HEAD must work too — GitHub camo / image tools preflight with HEAD and
	// chi would otherwise fall it through to the Bearer /api group (401).
	reqH := httptest.NewRequest(http.MethodHead, "/api/public/badge/demo", nil)
	recH := httptest.NewRecorder()
	r.ServeHTTP(recH, reqH)
	if recH.Code != http.StatusOK {
		t.Errorf("HEAD status = %d, want 200", recH.Code)
	}
	if ct := recH.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("HEAD Content-Type = %q, want image/svg+xml", ct)
	}

	// Unknown stack still returns a valid SVG.
	req2 := httptest.NewRequest(http.MethodGet, "/api/public/badge/ghost", nil)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if !strings.Contains(rec2.Body.String(), "unknown") {
		t.Errorf("ghost body missing unknown status:\n%s", rec2.Body.String())
	}

	// Service-level badge route.
	req3 := httptest.NewRequest(http.MethodGet, "/api/public/badge/demo/web", nil)
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, req3)
	if !strings.Contains(rec3.Body.String(), "demo/web: healthy") {
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
	if !strings.Contains(body5, "demo: healthy") || !strings.Contains(body5, "web: healthy") {
		t.Errorf("combined badge missing main + service segments:\n%s", body5)
	}
	if !strings.Contains(body5, "·") {
		t.Errorf("combined badge missing segment separator:\n%s", body5)
	}

	// Error snapshot renders the dark-red color.
	st2 := badgeStore(t, "error", map[string]string{"web": "error"})
	r2 := chi.NewRouter()
	BadgeMount(r2, st2)
	req6 := httptest.NewRequest(http.MethodGet, "/api/public/badge/demo", nil)
	rec6 := httptest.NewRecorder()
	r2.ServeHTTP(rec6, req6)
	if !strings.Contains(rec6.Body.String(), "error") || !strings.Contains(rec6.Body.String(), "#c62828") {
		t.Errorf("error snapshot body missing error/color:\n%s", rec6.Body.String())
	}
}

// TestStackBadge_FailoverDominates pins the derivation: an UNACKNOWLEDGED
// failover marker wins over the health snapshot ("failover"), an
// acknowledged marker falls through to the snapshot status, and a cleared
// (moved-back) marker does the same.
func TestStackBadge_FailoverDominates(t *testing.T) {
	ctx := context.Background()
	st := badgeStore(t, "healthy", map[string]string{"web": "healthy"})

	// Unacked marker dominates the healthy snapshot.
	if err := st.SetStackFailover(ctx, store.StackFailover{
		StackName: "demo", FromNode: "node-a", ToNode: "node-b", At: 1700000001,
	}); err != nil {
		t.Fatalf("SetStackFailover: %v", err)
	}
	if _, status := stackBadge(ctx, st, "demo"); status != "failover" {
		t.Errorf("unacked marker status = %q, want failover", status)
	}

	// Acknowledged → back to the snapshot's health.
	if err := st.AckStackFailover(ctx, "demo"); err != nil {
		t.Fatalf("AckStackFailover: %v", err)
	}
	if _, status := stackBadge(ctx, st, "demo"); status != "healthy" {
		t.Errorf("acked marker status = %q, want healthy", status)
	}

	// Cleared (moved back) → same: snapshot health only.
	if err := st.SetStackFailover(ctx, store.StackFailover{
		StackName: "demo", FromNode: "node-a", ToNode: "node-b", At: 1700000002,
	}); err != nil {
		t.Fatalf("SetStackFailover (reopen): %v", err)
	}
	if err := st.ClearStackFailover(ctx, "demo"); err != nil {
		t.Fatalf("ClearStackFailover: %v", err)
	}
	if _, status := stackBadge(ctx, st, "demo"); status != "healthy" {
		t.Errorf("cleared marker status = %q, want healthy", status)
	}

	// A stack with a marker but no snapshot still reads failover (the
	// marker is the stronger signal).
	if err := st.SetStackFailover(ctx, store.StackFailover{
		StackName: "ghost", FromNode: "node-a", ToNode: "node-b", At: 3,
	}); err != nil {
		t.Fatalf("SetStackFailover (ghost): %v", err)
	}
	if _, status := stackBadge(ctx, st, "ghost"); status != "failover" {
		t.Errorf("marker without snapshot status = %q, want failover", status)
	}
}

// TestBadgeHTTP_FailoverStatus drives the public badge endpoint through the
// whole failover lifecycle: unacked marker → amber "failover" SVG (both the
// single and the combined multi-segment badge), after ACK → the normal
// healthy green badge again.
func TestBadgeHTTP_FailoverStatus(t *testing.T) {
	ctx := context.Background()
	st := badgeStore(t, "healthy", map[string]string{"web": "healthy"})
	r := chi.NewRouter()
	BadgeMount(r, st)

	get := func(path string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200; body: %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	// Seed an open (unacknowledged) failover marker.
	if err := st.SetStackFailover(ctx, store.StackFailover{
		StackName: "demo", FromNode: "node-a", ToNode: "node-b", At: 1700000001,
	}); err != nil {
		t.Fatalf("SetStackFailover: %v", err)
	}

	body := get("/api/public/badge/demo")
	if !strings.Contains(body, "failover") {
		t.Errorf("unacked badge missing failover status:\n%s", body)
	}
	if !strings.Contains(body, "#e38b00") {
		t.Errorf("unacked badge missing amber failover color:\n%s", body)
	}
	if strings.Contains(body, "#44d47b") {
		t.Errorf("unacked badge must not read healthy/green:\n%s", body)
	}

	// The combined badge carries the same amber signal on its main segment.
	multi := get("/api/public/badge/demo/services")
	if !strings.Contains(multi, "failover") || !strings.Contains(multi, "#e38b00") {
		t.Errorf("combined badge missing failover segment/color:\n%s", multi)
	}

	// Acknowledge → the badge returns to the stack's normal health.
	if err := st.AckStackFailover(ctx, "demo"); err != nil {
		t.Fatalf("AckStackFailover: %v", err)
	}
	body = get("/api/public/badge/demo")
	if !strings.Contains(body, "healthy") || !strings.Contains(body, "#44d47b") {
		t.Errorf("acked badge missing healthy status/color:\n%s", body)
	}
	if strings.Contains(body, "failover") || strings.Contains(body, "#e38b00") {
		t.Errorf("acked badge still shows the failover signal:\n%s", body)
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
