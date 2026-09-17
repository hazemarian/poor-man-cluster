package backups

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mountBackups(h *HTTP) http.Handler {
	r := chi.NewRouter()
	h.Mount(r)
	h.MountStackScoped(r)
	return r
}

func TestBackupsAPI_PostCreatesAndRecords(t *testing.T) {
	st := newTestStore(t)
	h := &HTTP{Svc: NewLocal(st, func(_ context.Context) ([]string, error) {
		return []string{"/archive/x.tar.gz"}, nil
	})}
	srv := httptest.NewServer(mountBackups(h))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/backups", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "succeeded" {
		t.Errorf("status field = %v", body["status"])
	}

	rows, _ := st.ListBackups(context.Background(), 10)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Status != "succeeded" {
		t.Errorf("recorded status = %q", rows[0].Status)
	}
}

func TestBackupsAPI_PostFailureRecordsAndReturns502(t *testing.T) {
	st := newTestStore(t)
	h := &HTTP{Svc: NewLocal(st, func(_ context.Context) ([]string, error) {
		return nil, errors.New("offen blew up")
	})}
	srv := httptest.NewServer(mountBackups(h))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/backups", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}

	rows, _ := st.ListBackups(context.Background(), 10)
	if len(rows) != 1 || rows[0].Status != "failed" {
		t.Errorf("expected 1 failed row, got %+v", rows)
	}
}

func TestBackupsAPI_PostNoTriggerReturns503(t *testing.T) {
	st := newTestStore(t)
	h := &HTTP{Svc: NewLocal(st, nil)}
	srv := httptest.NewServer(mountBackups(h))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/backups", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestBackupsAPI_GetListsAndFiltersByStack(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.CreateBackup(ctx, "alpha", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateBackup(ctx, "beta", 1); err != nil {
		t.Fatal(err)
	}

	h := &HTTP{Svc: NewLocal(st, nil)}
	srv := httptest.NewServer(mountBackups(h))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/backups")
	if err != nil {
		t.Fatalf("get all: %v", err)
	}
	defer resp.Body.Close()
	var listAll struct {
		Backups []Run `json:"backups"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listAll); err != nil {
		t.Fatalf("decode all: %v", err)
	}
	if len(listAll.Backups) != 2 {
		t.Errorf("all returned %d, want 2", len(listAll.Backups))
	}

	resp2, err := http.Get(srv.URL + "/stacks/alpha/backups")
	if err != nil {
		t.Fatalf("get alpha: %v", err)
	}
	defer resp2.Body.Close()
	var listStack struct {
		Backups []Run `json:"backups"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&listStack); err != nil {
		t.Fatalf("decode stack: %v", err)
	}
	if len(listStack.Backups) != 1 || listStack.Backups[0].StackName != "alpha" {
		t.Errorf("alpha-scoped: %+v", listStack.Backups)
	}
}

func TestSplitArchivePaths(t *testing.T) {
	if got := splitArchivePaths(""); got != nil {
		t.Errorf("empty: %v", got)
	}
	got := splitArchivePaths("/a,/b,/c")
	if strings.Join(got, "|") != "/a|/b|/c" {
		t.Errorf("split: %v", got)
	}
}
