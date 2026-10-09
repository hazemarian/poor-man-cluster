package settings

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

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
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

// TestHTTP_GetMasksSecretsForNonAdmin is the FIX 5 guard: secret-ish keys
// (sso_client_secret, backup_s3_access_key/secret_key) are masked for non-admin
// bearers but kept in full for an admin. The CLI reads full values with the
// admin token.
func TestHTTP_GetMasksSecretsForNonAdmin(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for k, v := range map[string]string{
		"sso_client_secret":    "client-secret-value",
		"backup_s3_access_key": "ak-value",
		"backup_s3_secret_key": "sk-value",
		"domain":               "example.com",
		"volume_root":          "/data",
	} {
		if err := st.SetSetting(ctx, k, v); err != nil {
			t.Fatalf("SetSetting %s: %v", k, err)
		}
	}

	lookup := &fakeLookup{users: map[string]*auth.User{
		"admin-tok":  {ID: 1, Name: "admin", Role: auth.RoleAdmin},
		"viewer-tok": {ID: 2, Name: "viewer", Role: auth.RoleViewer},
	}}
	mux := chi.NewRouter()
	mux.Use(auth.Bearer(lookup))
	(&HTTP{Svc: NewLocal(st)}).Mount(mux)

	get := func(token string) map[string]string {
		req := httptest.NewRequest(http.MethodGet, "/cluster/settings", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET settings (token %q) code = %d, body=%s", token, rec.Code, rec.Body.String())
		}
		var body struct {
			Settings map[string]string `json:"settings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body.Settings
	}

	admin := get("admin-tok")
	if admin["sso_client_secret"] != "client-secret-value" {
		t.Errorf("admin sso_client_secret = %q, want full value", admin["sso_client_secret"])
	}
	if admin["backup_s3_access_key"] != "ak-value" {
		t.Errorf("admin backup_s3_access_key = %q, want full value", admin["backup_s3_access_key"])
	}
	if admin["domain"] != "example.com" {
		t.Errorf("admin domain = %q, want example.com", admin["domain"])
	}

	viewer := get("viewer-tok")
	for _, k := range []string{"sso_client_secret", "backup_s3_access_key", "backup_s3_secret_key"} {
		if viewer[k] != "********" {
			t.Errorf("viewer %s = %q, want masked", k, viewer[k])
		}
	}
	// Non-secret keys stay unmasked for the viewer.
	if viewer["domain"] != "example.com" || viewer["volume_root"] != "/data" {
		t.Errorf("viewer non-secret settings unexpectedly masked: %v", viewer)
	}
}

// fakeLookup is an auth.Lookup resolving bearer tokens to fixed roles.
type fakeLookup struct {
	users map[string]*auth.User
}

func (f *fakeLookup) UserByToken(_ context.Context, token string) (*auth.User, error) {
	return f.users[token], nil
}
