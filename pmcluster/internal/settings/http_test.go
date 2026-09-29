package settings

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func newHTTPEnv(t *testing.T) (*HTTP, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &HTTP{Svc: NewLocal(st)}, st
}

func settingsDo(t *testing.T, h *HTTP, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	mux := chi.NewRouter()
	h.Mount(mux)
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	if len(rec.Body.Bytes()) > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

func TestHTTP_GetReturnsAllAllowlistedKeys(t *testing.T) {
	h, _ := newHTTPEnv(t)

	rec, body := settingsDo(t, h, "GET", "/cluster/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get code = %d, body=%s", rec.Code, rec.Body.String())
	}
	settings, ok := body["settings"].(map[string]any)
	if !ok {
		t.Fatalf("settings = %v, want a map", body["settings"])
	}
	for _, key := range clusterSettingKeys {
		if _, present := settings[key]; !present {
			t.Errorf("allowlisted key %q missing from GET response", key)
		}
	}
}

func TestHTTP_PutRoundTrip(t *testing.T) {
	h, st := newHTTPEnv(t)
	ctx := t.Context()

	rec, body := settingsDo(t, h, "PUT", "/cluster/settings", `{"settings":{"volume_root":"/data","backup_retention_days":"30"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put code = %d, body=%s", rec.Code, rec.Body.String())
	}
	updated := body["settings"].(map[string]any)
	if updated["volume_root"] != "/data" || updated["backup_retention_days"] != "30" {
		t.Errorf("updated settings = %v, want volume_root=/data + retention=30", updated)
	}

	// Persisted in the store.
	if got := st.GetSettingDefault(ctx, "volume_root", ""); got != "/data" {
		t.Errorf("stored volume_root = %q, want /data", got)
	}
}

func TestHTTP_PutRejectsUnknownAndBadBody(t *testing.T) {
	h, _ := newHTTPEnv(t)

	// Unknown key → 400, nothing written.
	rec, body := settingsDo(t, h, "PUT", "/cluster/settings", `{"settings":{"bogus_key":"x"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown key code = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(body["error"].(string), "unknown") {
		t.Errorf("unknown-key error = %v, want mention of unknown", body["error"])
	}

	// Empty value is allowed (clears the setting) and round-trips.
	rec, body = settingsDo(t, h, "PUT", "/cluster/settings", `{"settings":{"domain":""}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear setting code = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if v := body["settings"].(map[string]any)["domain"]; v != "" {
		t.Errorf("cleared domain = %v, want empty", v)
	}

	// Malformed body → 400.
	rec, _ = settingsDo(t, h, "PUT", "/cluster/settings", `{nope`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body code = %d, want 400", rec.Code)
	}

	// Unknown top-level field → 400 (DisallowUnknownFields).
	rec, _ = settingsDo(t, h, "PUT", "/cluster/settings", `{"settings":{"domain":"x"},"extra":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown top-level code = %d, want 400", rec.Code)
	}
}
