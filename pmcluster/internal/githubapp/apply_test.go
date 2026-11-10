package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// applyFake serves exactly what Apply needs so its validation order can be
// tested without a live GitHub: a permissions read, an installation-token mint
// for the installations the test accepts, and one repository read.
type applyFake struct {
	srv *httptest.Server
	// minted counts access_tokens calls.
	minted atomic.Int32
	// probed counts repository reads.
	probed atomic.Int32
	// installations is the set that mints successfully.
	installations map[int64]bool
	// repoStatus, when non-zero, fails the repository probe with it.
	repoStatus int
}

func newApplyFake(t *testing.T, installations ...int64) *applyFake {
	t.Helper()
	f := &applyFake{installations: map[int64]bool{}}
	for _, id := range installations {
		f.installations[id] = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 4242, "slug": "pmcluster",
			"permissions": map[string]string{"contents": "read", "packages": "read"},
		})
	})
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		f.minted.Add(1)
		var id int64
		suffix := "/access_tokens"
		rest := strings.TrimPrefix(r.URL.Path, "/app/installations/")
		if !strings.HasSuffix(rest, suffix) {
			http.NotFound(w, r)
			return
		}
		raw := strings.TrimSuffix(rest, suffix)
		id, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil || !f.installations[id] {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "ghs_apply_fake", "expires_at": "2026-11-10T13:00:00Z",
		})
	})
	mux.HandleFunc("/repos/acme/app/contents/", func(w http.ResponseWriter, _ *http.Request) {
		f.probed.Add(1)
		if f.repoStatus != 0 {
			w.WriteHeader(f.repoStatus)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// client points a Client at the fake.
func (f *applyFake) client() *Client {
	return &Client{APIBase: f.srv.URL, HTTP: f.srv.Client()}
}

func probePtr(s string) *string { return &s }

func TestFingerprintIsStableAndDistinct(t *testing.T) {
	keyA := testKeyPEM(t)
	keyB := testKeyPEM(t)

	fpA, err := Fingerprint(keyA)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	again, err := Fingerprint(keyA)
	if err != nil {
		t.Fatalf("Fingerprint (again): %v", err)
	}
	if fpA != again {
		t.Errorf("the same key gave %q then %q", fpA, again)
	}
	fpB, err := Fingerprint(keyB)
	if err != nil {
		t.Fatalf("Fingerprint (other key): %v", err)
	}
	if fpA == fpB {
		t.Error("two different keys share a fingerprint")
	}
	if !strings.HasPrefix(fpA, "SHA256:") || strings.Count(fpA, ":") != 16 {
		t.Errorf("fingerprint %q is not in the SHA256:xx:… display shape", fpA)
	}
	if strings.Contains(fpA, "PRIVATE") {
		t.Error("the fingerprint leaks PEM material")
	}
}

func TestApply_PersistsAndReportsTheProbe(t *testing.T) {
	l, _, _ := newLocal(t)
	fake := newApplyFake(t, 7)
	l.Client = fake.client()
	ctx := context.Background()
	key := testKeyPEM(t)

	res, err := l.Apply(ctx, SetRequest{
		AppID: 4242, InstallationID: 7, PrivateKeyPEM: key,
		ProbeRepo: probePtr("acme/app"),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Rotated {
		t.Error("the first registration is not reported as a rotation")
	}
	if !res.Probe.OK || !strings.Contains(res.Probe.Detail, "acme/app") {
		t.Errorf("probe = %+v, want a successful read of acme/app", res.Probe)
	}
	wantFP, _ := Fingerprint(key)
	if res.KeyFingerprint != wantFP {
		t.Errorf("fingerprint = %q, want %q", res.KeyFingerprint, wantFP)
	}
	if fake.minted.Load() != 1 || fake.probed.Load() != 1 {
		t.Errorf("mints/probes = %d/%d, want 1/1", fake.minted.Load(), fake.probed.Load())
	}

	row, err := l.Store.GitHubApp(ctx)
	if err != nil {
		t.Fatalf("GitHubApp: %v", err)
	}
	if row.AppID != 4242 || row.InstallationID != 7 || row.ProbeRepo != "acme/app" {
		t.Errorf("stored row = %+v", row)
	}
	if row.KeyFingerprint != wantFP {
		t.Errorf("stored fingerprint = %q, want %q", row.KeyFingerprint, wantFP)
	}
	if row.LastProbeAt == 0 || !row.LastProbeOK {
		t.Errorf("the probe was not recorded: %+v", row)
	}
}

func TestApply_InstallationOnlyChangeKeepsTheKey(t *testing.T) {
	l, _, _ := newLocal(t)
	fake := newApplyFake(t, 7, 8)
	l.Client = fake.client()
	ctx := context.Background()
	key := testKeyPEM(t)
	if err := l.Save(ctx, 4242, 7, key, "acme/app", ""); err != nil {
		t.Fatalf("Save: %v", err)
	}
	before, err := Fingerprint(key)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	res, err := l.Apply(ctx, SetRequest{InstallationID: 8})
	if err != nil {
		t.Fatalf("Apply (installation only): %v", err)
	}
	if res.KeyFingerprint != before {
		t.Errorf("fingerprint = %q, want it unchanged at %q", res.KeyFingerprint, before)
	}
	if res.Rotated {
		t.Error("moving the installation id was reported as a key rotation")
	}
	if res.AppID != 4242 || res.InstallationID != 8 {
		t.Errorf("ids = %d/%d, want 4242/8", res.AppID, res.InstallationID)
	}

	row, err := l.Store.GitHubApp(ctx)
	if err != nil {
		t.Fatalf("GitHubApp: %v", err)
	}
	if row.InstallationID != 8 {
		t.Errorf("stored installation id = %d, want 8", row.InstallationID)
	}
	plain, err := l.decryptKey(row)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(plain) != string(key) {
		t.Error("the stored key changed during an installation-only update")
	}
	if row.ProbeRepo != "acme/app" {
		t.Errorf("probe repo = %q, want it kept when the request omits it", row.ProbeRepo)
	}
}

func TestApply_InstallationOnlyWithNoStoredKeyIsRefused(t *testing.T) {
	l, _, _ := newLocal(t)
	l.Client = newApplyFake(t, 7).client()

	_, err := l.Apply(context.Background(), SetRequest{AppID: 4242, InstallationID: 7})
	if !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("Apply = %v, want ErrKeyRequired", err)
	}
	if !strings.Contains(err.Error(), "key") {
		t.Errorf("error %q does not name the missing key", err)
	}
	if _, err := l.Store.GitHubApp(context.Background()); !errors.Is(err, store.ErrGitHubAppNotConfigured) {
		t.Errorf("a refused registration wrote a row: %v", err)
	}
}

func TestApply_BadInstallationLeavesTheRegistrationIntact(t *testing.T) {
	l, _, _ := newLocal(t)
	fake := newApplyFake(t, 7)
	l.Client = fake.client()
	ctx := context.Background()
	oldKey := testKeyPEM(t)
	if err := l.Save(ctx, 4242, 7, oldKey, "acme/app", ""); err != nil {
		t.Fatalf("Save: %v", err)
	}
	oldFP, _ := Fingerprint(oldKey)
	newKey := testKeyPEM(t)

	_, err := l.Apply(ctx, SetRequest{AppID: 4242, InstallationID: 99, PrivateKeyPEM: newKey})
	if err == nil {
		t.Fatal("Apply accepted an installation that cannot mint")
	}
	if !errors.Is(err, ErrInstallationNotFound) {
		t.Errorf("error = %v, want ErrInstallationNotFound", err)
	}

	row, err := l.Store.GitHubApp(ctx)
	if err != nil {
		t.Fatalf("GitHubApp: %v", err)
	}
	if row.AppID != 4242 || row.InstallationID != 7 {
		t.Errorf("ids = %d/%d, want the previous 4242/7", row.AppID, row.InstallationID)
	}
	if row.KeyFingerprint != oldFP {
		t.Errorf("fingerprint = %q, want the previous key %q", row.KeyFingerprint, oldFP)
	}
	plain, err := l.decryptKey(row)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(plain) != string(oldKey) {
		t.Error("a failed installation change overwrote the working key")
	}
	// The previous registration still mints.
	if _, err := l.Token(ctx); err != nil {
		t.Errorf("the previous key stopped working: %v", err)
	}
}

func TestApply_ProbeFailureDoesNotPersist(t *testing.T) {
	l, _, _ := newLocal(t)
	fake := newApplyFake(t, 7)
	fake.repoStatus = http.StatusForbidden
	l.Client = fake.client()
	key := testKeyPEM(t)

	_, err := l.Apply(context.Background(), SetRequest{
		AppID: 4242, InstallationID: 7, PrivateKeyPEM: key,
		ProbeRepo: probePtr("acme/app"),
	})
	if err == nil {
		t.Fatal("Apply persisted although the probe failed")
	}
	if !errors.Is(err, ErrInsufficientPermission) {
		t.Errorf("error = %v, want ErrInsufficientPermission", err)
	}
	if _, err := l.Store.GitHubApp(context.Background()); err == nil {
		t.Error("a failed probe left a registration behind")
	}
}

func TestApply_MalformedPEMNeverNamesTheKey(t *testing.T) {
	l, _, _ := newLocal(t)
	l.Client = newApplyFake(t, 7).client()

	const marker = "SUPERSECRETKEYMATERIAL"
	// A decodable PEM block whose type is not a private key, with the marker
	// trailing it: pem.Decode stops at the END line, so the marker is part of
	// what was submitted but must never reach the error.
	pemBytes := []byte("\n\n-----BEGIN CERTIFICATE-----\nbm90IGEga2V5\n-----END CERTIFICATE-----\n" + marker + "\n")
	_, err := l.Apply(context.Background(), SetRequest{
		AppID: 4242, InstallationID: 7,
		PrivateKeyPEM: pemBytes,
	})
	if err == nil {
		t.Fatal("Apply accepted a certificate as a private key")
	}
	if !strings.Contains(err.Error(), "line 3") || !strings.Contains(err.Error(), "unsupported block type") {
		t.Errorf("error %q does not name the line and the block type", err)
	}
	if strings.Contains(err.Error(), marker) {
		t.Error("the error quotes the key material")
	}
}
