package configs

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// httpTestEnv wires a real store-backed Local service under a chi router
// mounting the configs HTTP routes (same shape as the daemon's /api mount).
type httpTestEnv struct {
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
	mux := chi.NewRouter()
	(&HTTP{Svc: NewLocal(st)}).Mount(mux)
	return &httpTestEnv{mux: mux, st: st}
}

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

func TestHTTP_CreateGetUpdateRoundTrip(t *testing.T) {
	env := newHTTPTestEnv(t)

	// Create.
	rec, body := env.do(t, "POST", "/configs", `{"name":"app_cfg","scope":"service","stack":"demo","kind":"env","content":"KEY=value"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if body["name"] != "app_cfg" || body["kind"] != "env" {
		t.Errorf("create body = %v, want name/kind", body)
	}

	// Get returns content.
	rec, body = env.do(t, "GET", "/configs/app_cfg", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if body["content"] != "KEY=value" {
		t.Errorf("content = %v, want KEY=value", body["content"])
	}

	// Update content.
	rec, body = env.do(t, "PUT", "/configs/app_cfg", `{"content":"KEY=value2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if body["hash"] != store.ConfigHash("KEY=value2") {
		t.Errorf("update hash = %v, want hash of new content", body["hash"])
	}
}

func TestHTTP_VersionsAndRollback(t *testing.T) {
	env := newHTTPTestEnv(t)

	// Seed with two updates so a version history exists.
	if rec, _ := env.do(t, "POST", "/configs", `{"name":"C","scope":"service","content":"v1"}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed create: code=%d", rec.Code)
	}
	for _, content := range []string{"v2", "v3"} {
		if rec, _ := env.do(t, "PUT", "/configs/C", `{"content":"`+content+`"}`); rec.Code != http.StatusOK {
			t.Fatalf("seed update: code=%d", rec.Code)
		}
	}

	rec, body := env.do(t, "GET", "/configs/C/versions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("versions code = %d, body=%s", rec.Code, rec.Body.String())
	}
	vers, ok := body["versions"].([]any)
	if !ok || len(vers) < 2 {
		t.Fatalf("versions = %v, want a history of >= 2 entries", body["versions"])
	}

	// Roll back to the oldest recorded version.
	first := vers[0].(map[string]any)
	versionID := int64(first["id"].(float64))
	rec, body = env.do(t, "POST", "/configs/C/rollback", `{"version_id":`+itoa(versionID)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if body["rolled_back_to"] != float64(versionID) {
		t.Errorf("rolled_back_to = %v, want %d", body["rolled_back_to"], versionID)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

func TestHTTP_DeleteAndNotFound(t *testing.T) {
	env := newHTTPTestEnv(t)

	if rec, _ := env.do(t, "POST", "/configs", `{"name":"X","scope":"service","content":"v"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create code = %d", rec.Code)
	}

	rec, _ := env.do(t, "DELETE", "/configs/X", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete code = %d", rec.Code)
	}

	rec, body := env.do(t, "GET", "/configs/X", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete code = %d, want 404 (body=%v)", rec.Code, body)
	}

	// Update ghost → 404.
	rec, _ = env.do(t, "PUT", "/configs/GHOST", `{"content":"v"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("update ghost code = %d, want 404", rec.Code)
	}
}

func TestHTTP_ValidationAndConflict(t *testing.T) {
	env := newHTTPTestEnv(t)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing name", `{"scope":"service","content":"v"}`, http.StatusBadRequest},
		{"bad scope", `{"name":"S","scope":"global","content":"v"}`, http.StatusBadRequest},
		{"stack on cluster scope", `{"name":"S","scope":"cluster","stack":"x","content":"v"}`, http.StatusBadRequest},
		{"bad kind", `{"name":"S","scope":"service","kind":"weird","content":"v"}`, http.StatusBadRequest},
		{"malformed json", `{nope`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		rec, _ := env.do(t, "POST", "/configs", tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s: code = %d, want %d (body=%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}

	// Duplicate name → 409.
	if rec, _ := env.do(t, "POST", "/configs", `{"name":"S","scope":"service","content":"v"}`); rec.Code != http.StatusCreated {
		t.Fatalf("first create code = %d", rec.Code)
	}
	rec, body := env.do(t, "POST", "/configs", `{"name":"S","scope":"service","content":"v2"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate code = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(body["error"].(string), "already exists") {
		t.Errorf("409 error = %v, want 'already exists'", body["error"])
	}
}

func TestHTTP_RenderedListsSnapshots(t *testing.T) {
	env := newHTTPTestEnv(t)

	rec, body := env.do(t, "GET", "/cluster/rendered", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rendered code = %d, body=%s", rec.Code, rec.Body.String())
	}
	cfgs, ok := body["configs"].([]any)
	if !ok {
		t.Fatalf("rendered configs = %v, want an array", body["configs"])
	}
	if len(cfgs) != 0 {
		t.Errorf("rendered len = %d, want 0 for a fresh store", len(cfgs))
	}
}
