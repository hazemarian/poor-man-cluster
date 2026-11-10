package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// POST /api/github/app carries a private key, so these tests assert three
// things beyond the happy path: the key never comes back, it never reaches the
// log, and a request that fails validation changes nothing.

// setServer assembles the route the way the server does (Bearer + role are the
// server's job; this is the handler's own contract) and returns a client for it.
func setServer(t *testing.T, l *Local, log *zerolog.Logger) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	(&HTTP{App: l, Log: log}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// postSet posts a raw JSON body and returns the status and body.
func postSet(t *testing.T, srv *httptest.Server, body string) (int, string) {
	t.Helper()
	resp, err := srv.Client().Post(srv.URL+"/github/app", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /github/app: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, buf.String()
}

type setBody struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKeyPEM  string `json:"private_key_pem,omitempty"`
	ProbeRepo      string `json:"probe_repo,omitempty"`
	ProbePackage   string `json:"probe_package,omitempty"`
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

type setResponse struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	KeyFingerprint string `json:"key_fingerprint"`
	Probe          struct {
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	} `json:"probe"`
}

func TestHTTP_SetStoresTheKeyAndReturnsAFingerprint(t *testing.T) {
	l, _, _ := newLocal(t)
	fake := newApplyFake(t, 7)
	l.Client = fake.client()
	srv := setServer(t, l, nil)

	key := testKeyPEM(t)
	status, body := postSet(t, srv, mustJSON(t, setBody{
		AppID: 4242, InstallationID: 7, PrivateKeyPEM: string(key), ProbeRepo: "acme/app",
	}))
	if status != http.StatusOK {
		t.Fatalf("POST = %d, want 200; body: %s", status, body)
	}
	var got setResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantFP, _ := Fingerprint(key)
	if got.AppID != 4242 || got.InstallationID != 7 {
		t.Errorf("ids = %d/%d, want 4242/7", got.AppID, got.InstallationID)
	}
	if got.KeyFingerprint != wantFP {
		t.Errorf("fingerprint = %q, want %q", got.KeyFingerprint, wantFP)
	}
	if !got.Probe.OK || !strings.Contains(got.Probe.Detail, "acme/app") {
		t.Errorf("probe = %+v, want a successful read", got.Probe)
	}
	// The response must not carry the key in any form.
	if strings.Contains(body, "PRIVATE KEY") || strings.Contains(body, strings.Split(string(key), "\n")[1]) {
		t.Error("the response carries key material")
	}
	if strings.Contains(body, "key_pem") {
		t.Errorf("the response has a key field: %s", body)
	}

	// The same key gives the same fingerprint; the stored one matches.
	row, err := l.Store.GitHubApp(context.Background())
	if err != nil {
		t.Fatalf("GitHubApp: %v", err)
	}
	if row.KeyFingerprint != wantFP {
		t.Errorf("stored fingerprint = %q, want %q", row.KeyFingerprint, wantFP)
	}
	again, err := Fingerprint(mustDecrypt(t, l, row))
	if err != nil {
		t.Fatalf("Fingerprint (stored key): %v", err)
	}
	if again != wantFP {
		t.Errorf("the stored key's fingerprint = %q, want %q", again, wantFP)
	}
}

func mustDecrypt(t *testing.T, l *Local, row *store.GitHubApp) []byte {
	t.Helper()
	plain, err := l.decryptKey(row)
	if err != nil {
		t.Fatalf("decrypt stored key: %v", err)
	}
	return plain
}

func TestHTTP_SetInstallationOnlyKeepsTheStoredKey(t *testing.T) {
	l, _, _ := newLocal(t)
	fake := newApplyFake(t, 7, 8)
	l.Client = fake.client()
	ctx := context.Background()
	key := testKeyPEM(t)
	if err := l.Save(ctx, 4242, 7, key, "acme/app", ""); err != nil {
		t.Fatalf("Save: %v", err)
	}
	before, _ := Fingerprint(key)
	srv := setServer(t, l, nil)

	status, body := postSet(t, srv, `{"installation_id":8}`)
	if status != http.StatusOK {
		t.Fatalf("installation-only POST = %d, want 200; body: %s", status, body)
	}
	var got setResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.KeyFingerprint != before {
		t.Errorf("fingerprint = %q, want the unchanged %q", got.KeyFingerprint, before)
	}
	if got.InstallationID != 8 || got.AppID != 4242 {
		t.Errorf("ids = %d/%d, want 4242/8", got.AppID, got.InstallationID)
	}
	row, err := l.Store.GitHubApp(ctx)
	if err != nil {
		t.Fatalf("GitHubApp: %v", err)
	}
	if string(mustDecrypt(t, l, row)) != string(key) {
		t.Error("the stored key changed during an installation-only update")
	}
	if row.ProbeRepo != "acme/app" {
		t.Errorf("probe repo = %q, want it kept when the body omits it", row.ProbeRepo)
	}
}

func TestHTTP_SetInstallationOnlyWithoutAStoredKeyIs400(t *testing.T) {
	l, _, _ := newLocal(t)
	l.Client = newApplyFake(t, 7).client()
	srv := setServer(t, l, nil)

	status, body := postSet(t, srv, `{"app_id":4242,"installation_id":7}`)
	if status != http.StatusBadRequest {
		t.Fatalf("POST without a key = %d, want 400; body: %s", status, body)
	}
	if !strings.Contains(body, "key") {
		t.Errorf("the refusal does not name the missing key: %s", body)
	}
	if _, err := l.Store.GitHubApp(context.Background()); !errors.Is(err, store.ErrGitHubAppNotConfigured) {
		t.Errorf("a refused registration wrote a row: %v", err)
	}
}

func TestHTTP_SetBadInstallationKeepsThePreviousRegistration(t *testing.T) {
	l, _, _ := newLocal(t)
	fake := newApplyFake(t, 7)
	l.Client = fake.client()
	ctx := context.Background()
	oldKey := testKeyPEM(t)
	if err := l.Save(ctx, 4242, 7, oldKey, "acme/app", ""); err != nil {
		t.Fatalf("Save: %v", err)
	}
	oldFP, _ := Fingerprint(oldKey)
	srv := setServer(t, l, nil)

	newKey := testKeyPEM(t)
	status, body := postSet(t, srv, mustJSON(t, setBody{
		AppID: 4242, InstallationID: 99, PrivateKeyPEM: string(newKey),
	}))
	if status != http.StatusBadRequest {
		t.Fatalf("POST with a dead installation = %d, want 400; body: %s", status, body)
	}

	row, err := l.Store.GitHubApp(ctx)
	if err != nil {
		t.Fatalf("GitHubApp: %v", err)
	}
	if row.AppID != 4242 || row.InstallationID != 7 || row.KeyFingerprint != oldFP {
		t.Errorf("registration = %+v, want the previous 4242/7/%s", row, oldFP)
	}
	if string(mustDecrypt(t, l, row)) != string(oldKey) {
		t.Error("a rejected installation change overwrote the working key")
	}
	if _, err := l.Token(ctx); err != nil {
		t.Errorf("the previous key stopped working: %v", err)
	}
}

func TestHTTP_SetMalformedPEMLeaksNothingAndPersistsNothing(t *testing.T) {
	l, _, _ := newLocal(t)
	l.Client = newApplyFake(t, 7).client()

	var logs bytes.Buffer
	lg := zerolog.New(&logs)
	srv := setServer(t, l, &lg)

	const marker = "SUPERSECRETKEYMATERIAL"
	// Decodable PEM, wrong block type, marker trailing the block: the request
	// body really does contain the marker, so "not in the log or the response"
	// is a meaningful assertion.
	pemBytes := "-----BEGIN CERTIFICATE-----\nbm90IGEga2V5\n-----END CERTIFICATE-----\n" + marker + "\n"
	status, body := postSet(t, srv, mustJSON(t, setBody{
		AppID: 4242, InstallationID: 7, PrivateKeyPEM: pemBytes,
	}))
	if status != http.StatusBadRequest {
		t.Fatalf("POST with a certificate = %d, want 400; body: %s", status, body)
	}
	for what, got := range map[string]string{"response": body, "log": logs.String()} {
		if strings.Contains(got, marker) {
			t.Errorf("the %s carries key material: %s", what, got)
		}
		if strings.Contains(got, "BEGIN CERTIFICATE") {
			t.Errorf("the %s quotes the PEM: %s", what, got)
		}
	}
	if !strings.Contains(body, "unsupported block type") {
		t.Errorf("the refusal does not say what was wrong: %s", body)
	}
	if logs.Len() == 0 {
		t.Error("the refusal was not audited")
	}
	if _, err := l.Store.GitHubApp(context.Background()); !errors.Is(err, store.ErrGitHubAppNotConfigured) {
		t.Errorf("a refused registration wrote a row: %v", err)
	}
}

func TestHTTP_SetRefusesCleartextFromANonLoopbackCaller(t *testing.T) {
	l, _, _ := newLocal(t)
	l.Client = newApplyFake(t, 7).client()
	h := &HTTP{App: l}

	body := mustJSON(t, setBody{AppID: 4242, InstallationID: 7, PrivateKeyPEM: string(testKeyPEM(t))})
	req := httptest.NewRequest(http.MethodPost, "/api/github/app", strings.NewReader(body))
	req.RemoteAddr = "203.0.113.9:5555" // the test server's real IP, not loopback
	rr := httptest.NewRecorder()
	h.set(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("cleartext POST = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "insecure_transport") {
		t.Errorf("the refusal does not name the reason: %s", rr.Body.String())
	}
	if _, err := l.Store.GitHubApp(context.Background()); !errors.Is(err, store.ErrGitHubAppNotConfigured) {
		t.Errorf("a cleartext submit wrote a row: %v", err)
	}

	// The edge terminates TLS and forwards the scheme; that is accepted.
	req = httptest.NewRequest(http.MethodPost, "/api/github/app", strings.NewReader(body))
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("X-Forwarded-Proto", "https")
	rr = httptest.NewRecorder()
	h.set(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST with X-Forwarded-Proto: https = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	// A loopback caller (the CLI on the leader, the console beside the daemon)
	// needs no TLS: the key never crosses a network.
	req = httptest.NewRequest(http.MethodPost, "/api/github/app", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:4711"
	rr = httptest.NewRecorder()
	h.set(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("loopback POST = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
}

func TestHTTP_SetRejectsAnOversizedBody(t *testing.T) {
	l, _, _ := newLocal(t)
	l.Client = newApplyFake(t, 7).client()
	srv := setServer(t, l, nil)

	big := mustJSON(t, setBody{
		AppID: 4242, InstallationID: 7,
		PrivateKeyPEM: strings.Repeat("A", maxSetBodyBytes+1024),
	})
	status, body := postSet(t, srv, big)
	if status != http.StatusBadRequest {
		t.Fatalf("POST with a %d-byte body = %d, want 400; body: %s", len(big), status, body)
	}
	if _, err := l.Store.GitHubApp(context.Background()); !errors.Is(err, store.ErrGitHubAppNotConfigured) {
		t.Errorf("an oversized body wrote a row: %v", err)
	}
}
