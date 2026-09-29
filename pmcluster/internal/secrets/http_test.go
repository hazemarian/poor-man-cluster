package secrets

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// httpTestEnv wires a real store + cipher-backed Local service under a chi
// router mounting the secrets HTTP routes, mirroring how the daemon mounts
// them under /api.
type httpTestEnv struct {
	svc *Local
	mux chi.Router
	st  *store.Store
}

func newHTTPTestEnv(t *testing.T) *httpTestEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c, err := credentials.Open(filepath.Join(t.TempDir(), ".encryption_key"))
	if err != nil {
		t.Fatalf("credentials.Open: %v", err)
	}
	svc := NewLocal(st, c)
	mux := chi.NewRouter()
	(&HTTP{Svc: svc}).Mount(mux)
	return &httpTestEnv{svc: svc, mux: mux, st: st}
}

// do performs a JSON request against the mounted router and returns the raw
// response plus the decoded error envelope (when present).
func (e *httpTestEnv) do(t *testing.T, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)

	var payload map[string]any
	if len(rec.Body.Bytes()) > 0 && strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	}
	return rec, payload
}

func TestHTTP_CreateGetValueRoundTrip(t *testing.T) {
	env := newHTTPTestEnv(t)

	// Create.
	rec, body := env.do(t, "POST", "/secrets", `{"name":"DB_PASS","scope":"service","stack":"mystack","value":"hunter2"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if body["name"] != "DB_PASS" || body["scope"] != "service" {
		t.Errorf("create body = %v, want name/scope", body)
	}

	// List (metadata only — value must never leak).
	rec, body = env.do(t, "GET", "/secrets?scope=service&stack=mystack", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list code = %d, body=%s", rec.Code, rec.Body.String())
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "hunter2") {
		t.Error("list response leaked the plaintext secret value")
	}

	// Value (operator reveal).
	rec, body = env.do(t, "GET", "/secrets/DB_PASS/value", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("value code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if body["value"] != "hunter2" {
		t.Errorf("value = %v, want hunter2", body["value"])
	}
}

func TestHTTP_UpdateAndDelete(t *testing.T) {
	env := newHTTPTestEnv(t)
	ctx := context.Background()

	if _, err := env.svc.Create(ctx, "service", "", "TOKEN", "old"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Update value.
	rec, body := env.do(t, "PUT", "/secrets/TOKEN", `{"value":"new"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if body["hash"] != store.SecretHash("new") {
		t.Errorf("update hash = %v, want hash of new", body["hash"])
	}

	// Reveal confirms new value.
	rec, body = env.do(t, "GET", "/secrets/TOKEN/value", "")
	if rec.Code != http.StatusOK || body["value"] != "new" {
		t.Fatalf("reveal after update: code=%d body=%v", rec.Code, body)
	}

	// Delete.
	rec, _ = env.do(t, "DELETE", "/secrets/TOKEN", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete code = %d, body=%s", rec.Code, rec.Body.String())
	}

	// Value after delete → 404.
	rec, body = env.do(t, "GET", "/secrets/TOKEN/value", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("value after delete code = %d, want 404, body=%v", rec.Code, body)
	}
	if !strings.Contains(body["error"].(string), "not found") {
		t.Errorf("404 error = %v, want 'not found'", body["error"])
	}
}

func TestHTTP_ValidationErrors(t *testing.T) {
	env := newHTTPTestEnv(t)
	cases := []struct {
		name string
		path string
		body string
		want int
	}{
		{"missing name", "/secrets", `{"scope":"service","value":"v"}`, http.StatusBadRequest},
		{"bad scope", "/secrets", `{"name":"S","scope":"global","value":"v"}`, http.StatusBadRequest},
		{"stack on cluster scope", "/secrets", `{"name":"S","scope":"cluster","stack":"x","value":"v"}`, http.StatusBadRequest},
		{"empty value", "/secrets", `{"name":"S","scope":"service","value":""}`, http.StatusBadRequest},
		{"malformed JSON", "/secrets", `{not-json`, http.StatusBadRequest},
		{"unknown field", "/secrets", `{"name":"S","scope":"service","value":"v","bogus":1}`, http.StatusBadRequest},
		{"update empty value", "/secrets/S", `{"value":""}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		rec, _ := env.do(t, "POST", tc.path, tc.body)
		if tc.path == "/secrets/S" {
			rec, _ = env.do(t, "PUT", tc.path, tc.body)
		}
		if rec.Code != tc.want {
			t.Errorf("%s: code = %d, want %d (body=%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestHTTP_CreateConflictAndNotFound(t *testing.T) {
	env := newHTTPTestEnv(t)

	// First create succeeds.
	if rec, _ := env.do(t, "POST", "/secrets", `{"name":"S","scope":"service","value":"v"}`); rec.Code != http.StatusCreated {
		t.Fatalf("first create code = %d", rec.Code)
	}

	// Duplicate name → 409.
	rec, body := env.do(t, "POST", "/secrets", `{"name":"S","scope":"service","value":"v2"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate code = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(body["error"].(string), "already exists") {
		t.Errorf("409 error = %v, want 'already exists'", body["error"])
	}

	// Update unknown → 404.
	rec, _ = env.do(t, "PUT", "/secrets/GHOST", `{"value":"v"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("update ghost code = %d, want 404", rec.Code)
	}
}
