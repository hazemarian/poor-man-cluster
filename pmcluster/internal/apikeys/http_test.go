package apikeys

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func newTestHTTP(t *testing.T) (*HTTP, *store.Store, *chi.Mux) {
	t.Helper()
	st := openTestStore(t)
	svc := NewLocal(st)
	r := chi.NewRouter()
	(&HTTP{Svc: svc}).Mount(r)
	return &HTTP{Svc: svc}, st, r
}

func doJSON(t *testing.T, r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body == "" {
		rd = bytes.NewReader(nil)
	} else {
		rd = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return m
}

func TestHTTPListEmptyAndAfterCreate(t *testing.T) {
	_, st, r := newTestHTTP(t)
	ctx := context.Background()

	rec := doJSON(t, r, http.MethodGet, "/api_keys", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api_keys empty = %d, want 200", rec.Code)
	}
	m := decodeBody(t, rec)
	if keys, ok := m["keys"].([]any); !ok || len(keys) != 0 {
		t.Errorf("empty list = %#v, want []", m["keys"])
	}

	// Create one, then list again.
	svc := NewLocal(st)
	if _, _, err := svc.Create(ctx, "alice"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec = doJSON(t, r, http.MethodGet, "/api_keys", "")
	m = decodeBody(t, rec)
	keys := m["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("list = %d keys, want 1", len(keys))
	}
	first := keys[0].(map[string]any)
	if first["name"] != "alice" {
		t.Errorf("first key name = %v, want alice", first["name"])
	}
}

func TestHTTPCreateHappyAndResponseShape(t *testing.T) {
	_, _, r := newTestHTTP(t)

	rec := doJSON(t, r, http.MethodPost, "/api_keys", `{"name":"bob"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api_keys = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	m := decodeBody(t, rec)
	if m["name"] != "bob" || m["stack"] != "" {
		t.Errorf("create response = %#v, want name bob/stack empty", m)
	}
	tok, _ := m["token"].(string)
	if !strings.HasPrefix(tok, "pmc_") {
		t.Errorf("token = %q, want pmc_ prefix", tok)
	}
	if id, ok := m["id"].(float64); !ok || id <= 0 {
		t.Errorf("id = %#v, want > 0", m["id"])
	}
}

func TestHTTPCreateStackScoped(t *testing.T) {
	_, _, r := newTestHTTP(t)

	rec := doJSON(t, r, http.MethodPost, "/api_keys", `{"name":"deployer","stack":"demo"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	m := decodeBody(t, rec)
	if m["stack"] != "demo" {
		t.Errorf("stack = %v, want demo", m["stack"])
	}
}

func TestHTTPCreateValidationErrors(t *testing.T) {
	_, _, r := newTestHTTP(t)

	// Malformed JSON.
	rec := doJSON(t, r, http.MethodPost, "/api_keys", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON = %d, want 400", rec.Code)
	}

	// Unknown fields are rejected.
	rec = doJSON(t, r, http.MethodPost, "/api_keys", `{"name":"bob","extra":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field = %d, want 400", rec.Code)
	}

	// Missing name.
	rec = doJSON(t, r, http.MethodPost, "/api_keys", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty body = %d, want 400", rec.Code)
	}
	rec = doJSON(t, r, http.MethodPost, "/api_keys", `{"name":"  "}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("blank name = %d, want 400", rec.Code)
	}
}

func TestHTTPCreateDuplicateUserConflict(t *testing.T) {
	_, _, r := newTestHTTP(t)

	doJSON(t, r, http.MethodPost, "/api_keys", `{"name":"bob"}`)
	rec := doJSON(t, r, http.MethodPost, "/api_keys", `{"name":"bob"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate create = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "user already exists") {
		t.Errorf("conflict body = %s, want mention of duplicate", rec.Body.String())
	}
}

func TestHTTPRemoveLifecycle(t *testing.T) {
	_, st, r := newTestHTTP(t)
	ctx := context.Background()
	svc := NewLocal(st)
	id, _, err := svc.Create(ctx, "alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rec := doJSON(t, r, http.MethodDelete, "/api_keys/"+itoa(id), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
}

func TestHTTPRemoveErrors(t *testing.T) {
	_, _, r := newTestHTTP(t)

	// Non-numeric / non-positive id.
	for _, p := range []string{"/api_keys/abc", "/api_keys/0", "/api_keys/-5"} {
		rec := doJSON(t, r, http.MethodDelete, p, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("DELETE %s = %d, want 400", p, rec.Code)
		}
	}

	// Missing id.
	rec := doJSON(t, r, http.MethodDelete, "/api_keys/9999", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("DELETE unknown = %d, want 404", rec.Code)
	}
}

func TestHTTPRemoveEdgeUserConflict(t *testing.T) {
	_, st, r := newTestHTTP(t)
	ctx := context.Background()
	svc := NewLocal(st)
	id, _, err := svc.Create(ctx, "edge")
	if err != nil {
		t.Fatalf("Create edge: %v", err)
	}

	rec := doJSON(t, r, http.MethodDelete, "/api_keys/"+itoa(id), "")
	if rec.Code != http.StatusConflict {
		t.Errorf("DELETE edge = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "edge") {
		t.Errorf("conflict body = %s, want edge mention", rec.Body.String())
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

func TestHTTPCreateUsesFirstNonEmptyStack(t *testing.T) {
	_, _, r := newTestHTTP(t)
	rec := doJSON(t, r, http.MethodPost, "/api_keys", `{"name":"x","stack":"  demo  "}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	m := decodeBody(t, rec)
	if m["stack"] != "demo" {
		t.Errorf("stack = %q, want trimmed demo", m["stack"])
	}
}
