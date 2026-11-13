package nodereport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth/swarmtls"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "node-report.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// withNode attaches the identity the swarm-TLS verifier puts on the request.
func withNode(r *http.Request, host string) *http.Request {
	return r.WithContext(swarmtls.ContextWithNode(r.Context(), swarmtls.Node{ID: "id-" + host, Hostname: host}))
}

func post(t *testing.T, s *Service, host, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(body))
	if host != "" {
		req = withNode(req, host)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// TestReportStoresTheRowAndTheCertificateNamesIt: the body is a measurement, not an
// identity. A node that claims another hostname is corrected — otherwise one machine can
// overwrite another's row and every panel built on it lies.
func TestReportStoresTheRowAndTheCertificateNamesIt(t *testing.T) {
	st := newStore(t)
	svc := &Service{Store: st, Log: zerolog.Nop(), Now: func() time.Time { return time.Unix(1700000000, 0) }}

	rec := post(t, svc, "nxt-sw-6-w", `{"hostname":"nxt-sw-1-m","cores":2,"load1":0.5,"mem_total_mb":3852,"mem_used_mb":1200,"mem_avail_mb":2652,"docker_up":1,"ts":1700000000}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ignored":"nxt-sw-1-m"`) {
		t.Fatalf("a mismatched claim was not reported: %s", rec.Body.String())
	}

	rows, err := st.ListNodeStats(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Hostname != "nxt-sw-6-w" {
		t.Fatalf("stored under %q, want the CERTIFICATE's node nxt-sw-6-w", rows[0].Hostname)
	}
	if rows[0].MemTotalMB != 3852 || rows[0].DockerUp != 1 {
		t.Fatalf("the measurement did not survive: %+v", rows[0])
	}
}

// TestReportReplacesTheNodesRow: one row per node, rewritten in place. A metrics history
// in the control-plane SQLite file is the thing this table exists to avoid.
func TestReportReplacesTheNodesRow(t *testing.T) {
	st := newStore(t)
	svc := &Service{Store: st, Log: zerolog.Nop()}

	post(t, svc, "nxt-sw-6-w", `{"mem_total_mb":3852,"mem_used_mb":1000,"docker_up":1}`)
	post(t, svc, "nxt-sw-6-w", `{"mem_total_mb":3852,"mem_used_mb":2000,"docker_up":0}`)

	rows, err := st.ListNodeStats(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (upsert, not append)", len(rows))
	}
	if rows[0].MemUsedMB != 2000 || rows[0].DockerUp != 0 {
		t.Fatalf("the row was not replaced by the newest sample: %+v", rows[0])
	}
}

// TestReportRefusesAnUnidentifiedCaller: the listener runs the swarm-TLS middleware, and
// a report without a node certificate must never reach the store.
func TestReportRefusesAnUnidentifiedCaller(t *testing.T) {
	svc := &Service{Store: newStore(t), Log: zerolog.Nop()}
	if rec := post(t, svc, "", `{"mem_total_mb":1}`); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req := httptest.NewRequest(method, Path, nil)
		req = withNode(req, "nxt-sw-6-w")
		rec := httptest.NewRecorder()
		svc.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want 405 — the report is POST only", method, rec.Code)
		}
	}
}
