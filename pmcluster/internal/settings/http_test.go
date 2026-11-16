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
	if got := st.SettingDefault(ctx, "volume_root", ""); got != "/data" {
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

// fakeLookup is an auth.Lookup resolving bearer tokens to fixed roles.
type fakeLookup struct {
	users map[string]*auth.User
}

func (f *fakeLookup) UserByToken(_ context.Context, token string) (*auth.User, error) {
	return f.users[token], nil
}

// TestHTTP_GetNeverReturnsSecrets replaces "masked for non-admins, cleartext for admins".
// The unit of exposure is not the bearer, it is the BROWSER: the console holds the admin
// token, so an admin-cleartext response puts the S3 secret and the SSO client secret into
// a page, a devtools panel and the browser's memory. Nobody needs to read back a secret
// they stored; they need to know whether one is set.
func TestHTTP_GetNeverReturnsSecrets(t *testing.T) {
	h, st := newHTTPEnv(t)
	for k, v := range map[string]string{
		"log_level":            "info",
		"sso_client_secret":    "super-secret",
		"backup_s3_access_key": "AKIAEXAMPLE",
		"backup_s3_secret_key": "shhh",
		"backup_s3_bucket":     "backups",
	} {
		if err := st.SetSetting(context.Background(), k, v); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}

	rec, _ := settingsDo(t, h, http.MethodGet, "/cluster/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Settings  map[string]string `json:"settings"`
		SecretSet map[string]bool   `json:"secret_set"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"sso_client_secret", "backup_s3_access_key", "backup_s3_secret_key"} {
		if body.Settings[k] != Mask {
			t.Fatalf("%s = %q, want the mask", k, body.Settings[k])
		}
		if !body.SecretSet[k] {
			t.Fatalf("secret_set[%s] = false, want true: the form must know one is stored", k)
		}
	}
	if body.Settings["backup_s3_bucket"] != "backups" {
		t.Fatalf("a non-secret value was masked: %q", body.Settings["backup_s3_bucket"])
	}
	if strings.Contains(rec.Body.String(), "super-secret") || strings.Contains(rec.Body.String(), "AKIAEXAMPLE") {
		t.Fatal("a secret value appeared in the response body")
	}
}

// TestHTTP_PutLeavesMaskedSecretsAlone: a form rendered from a masked GET submits the mask
// back. Saving must not erase the secret the page was never shown.
func TestHTTP_PutLeavesMaskedSecretsAlone(t *testing.T) {
	h, st := newHTTPEnv(t)
	ctx := context.Background()
	if err := st.SetSetting(ctx, "sso_client_secret", "super-secret"); err != nil {
		t.Fatal(err)
	}
	rec, _ := settingsDo(t, h, http.MethodPut, "/cluster/settings",
		`{"settings":{"sso_client_secret":"`+Mask+`","log_level":"debug"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := st.SettingDefault(ctx, "sso_client_secret", ""); got != "super-secret" {
		t.Fatalf("the secret was overwritten by the mask: %q", got)
	}
	if got := st.SettingDefault(ctx, "log_level", ""); got != "debug" {
		t.Fatalf("log_level = %q, want debug — a normal setting must still apply", got)
	}
}
