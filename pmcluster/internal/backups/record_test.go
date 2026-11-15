package backups

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// A node posts the artifact it just confirmed; the leader writes the row with the
// stack attached. This is the path that replaces "a scan discovered it later,
// without the stack".
func TestRecordServiceWritesTheRow(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer func() { _ = st.Close() }()
	svc := &RecordService{Store: st, Log: zerolog.Nop()}

	body, _ := json.Marshal(store.ArtifactRecord{
		Stack: "shop", Node: "n1", Archive: "backup-n1-2026-11-15T07-00-00-stack-shop.tar.gz",
		Status: "succeeded", Files: 3, Bytes: 4096, SHA256: "abc", CreatedAt: 1794722452,
	})
	req := httptest.NewRequest(http.MethodPost, RecordPath, strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	svc.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}

	rows, err := st.ListBackups(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if !rows[0].StackName.Valid || rows[0].StackName.String != "shop" {
		t.Fatalf("row has no stack: %+v", rows[0].StackName)
	}
	if rows[0].Status != "succeeded" {
		t.Errorf("status = %q", rows[0].Status)
	}

	// Idempotent: the same record again (a retry, or discovery later) updates the
	// row instead of duplicating it.
	req2 := httptest.NewRequest(http.MethodPost, RecordPath, strings.NewReader(string(body)))
	svc.ServeHTTP(httptest.NewRecorder(), req2)
	rows, _ = st.ListBackups(context.Background(), 10)
	if len(rows) != 1 {
		t.Fatalf("retry duplicated the row: %d rows", len(rows))
	}
}

// A record without a stack or an archive is refused: a nameless row is exactly
// the defect this endpoint exists to remove.
func TestRecordServiceRefusesNamelessRecords(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer func() { _ = st.Close() }()
	svc := &RecordService{Store: st, Log: zerolog.Nop()}
	for _, body := range []string{`{"archive":"a.tar.gz"}`, `{"stack":"shop"}`, `not json`} {
		rec := httptest.NewRecorder()
		svc.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RecordPath, strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q accepted with %d", body, rec.Code)
		}
	}
	// Only POST is a record; a GET is not a way to write anything.
	rec := httptest.NewRecorder()
	svc.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, RecordPath, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rec.Code)
	}
}
