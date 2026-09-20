package certs

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// openStore opens a fresh store in a temp dir for one test.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// genCert mints a self-signed ECDSA certificate covering the given host.
func genCert(t *testing.T, host string) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}

// fakeService implements certs.Service against a real store so List/Get
// reflect what the handlers persisted. It records applies/removes and can be
// forced to fail.
type fakeService struct {
	store      *store.Store
	main       string
	refreshes  int
	applyCalls int
	fail       error
}

func (f *fakeService) SiteCert(ctx context.Context, domain, certPEM, keyPEM string) (*Cert, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.applyCalls++
	f.refreshes++
	now := time.Now().UTC()
	row := store.SiteCertRow{
		Domain:     domain,
		CertSecret: "cert_v042",
		KeySecret:  "key_v042",
		NotBefore:  now.Add(-time.Hour),
		NotAfter:   now.Add(24 * time.Hour),
		SANs:       []string{domain},
		CertHash:   store.ConfigHash(certPEM),
		KeyHash:    store.ConfigHash(keyPEM),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := f.store.PutSiteCert(ctx, row); err != nil {
		return nil, err
	}
	return rowModel(&row), nil
}

func (f *fakeService) ApplyHostCert(ctx context.Context, host, certPEM, keyPEM string, _ bool) (*Cert, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	if strings.Contains(host, "/") || strings.Contains(host, "..") {
		return nil, errors.New("invalid host")
	}
	if !strings.Contains(certPEM, "BEGIN CERTIFICATE") {
		return nil, errors.New("not a valid PEM pair")
	}
	f.applyCalls++
	f.refreshes++
	now := time.Now().UTC()
	row := store.SiteCertRow{
		Domain:     host,
		CertSecret: "hostcert-" + host + "_v001",
		KeySecret:  "hostkey-" + host + "_v001",
		NotBefore:  now.Add(-time.Hour),
		NotAfter:   now.Add(24 * time.Hour),
		SANs:       []string{host},
		CertHash:   store.ConfigHash(certPEM),
		KeyHash:    store.ConfigHash(keyPEM),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := f.store.PutSiteCert(ctx, row); err != nil {
		return nil, err
	}
	return rowModel(&row), nil
}

func (f *fakeService) RemoveHostCert(ctx context.Context, host string, _ bool) error {
	if f.fail != nil {
		return f.fail
	}
	f.refreshes++
	if _, err := f.store.GetSiteCert(ctx, host); err != nil {
		return err
	}
	return f.store.DeleteSiteCert(ctx, host)
}

func (f *fakeService) GetSiteCert(ctx context.Context, domain string) (*Cert, error) {
	row, err := f.store.GetSiteCert(ctx, domain)
	if err != nil {
		return nil, err
	}
	return rowModel(&row), nil
}

func (f *fakeService) List(ctx context.Context) ([]Cert, error) {
	rows, err := f.store.ListSiteCerts(ctx)
	if err != nil {
		return nil, err
	}
	return rowModels(rows), nil
}

func (f *fakeService) MainDomain(ctx context.Context) (string, error) {
	return f.main, nil
}

// mountRoutes mounts the handler on a chi router behind a minimal Bearer
// middleware (token "tok" passes) so the 401 subtests stay meaningful.
func mountRoutes(h *HTTP) http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer tok") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Route("/api", func(r chi.Router) {
		h.MountHosts(r)
		h.MountSite(r)
	})
	return r
}

func doJSON(t *testing.T, method, url, token string, body any) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b := make([]byte, 0)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			break
		}
	}
	return string(b)
}

func TestSiteCertAPI(t *testing.T) {
	st := openStore(t)
	rec := &fakeService{store: st, main: "test.example.com"}
	srv := httptest.NewServer(mountRoutes(&HTTP{Svc: rec}))
	defer srv.Close()
	base := srv.URL + "/api/tls/site"
	cert, key := genCert(t, "test.example.com")

	t.Run("unauth → 401", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("get before apply → 404", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "tok", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body: %s", resp.StatusCode, readBody(t, resp))
		}
	})

	t.Run("put missing key → 400", func(t *testing.T) {
		resp := doJSON(t, http.MethodPut, base, "tok", map[string]any{"cert": cert})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, readBody(t, resp))
		}
	})

	t.Run("put apply error → 500", func(t *testing.T) {
		rec.fail = errors.New("boom")
		defer func() { rec.fail = nil }()
		resp := doJSON(t, http.MethodPut, base, "tok", map[string]any{"cert": cert, "key": key})
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body: %s", resp.StatusCode, readBody(t, resp))
		}
	})

	t.Run("put success → 200", func(t *testing.T) {
		resp := doJSON(t, http.MethodPut, base, "tok", map[string]any{"cert": cert, "key": key})
		b := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
		}
		var got siteCertResponse
		if err := json.Unmarshal([]byte(b), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Domain != "test.example.com" || got.CertSecret == "" || got.NotAfter == "" {
			t.Errorf("unexpected response: %+v", got)
		}
		if rec.applyCalls != 1 {
			t.Errorf("apply calls = %d, want 1", rec.applyCalls)
		}
	})

	t.Run("get after apply → 200", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "tok", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, readBody(t, resp))
		}
	})
}

func TestHostCertAPI_CRUDAndAuth(t *testing.T) {
	st := openStore(t)
	rec := &fakeService{store: st, main: "nextrum-sy.com"}
	srv := httptest.NewServer(mountRoutes(&HTTP{Svc: rec}))
	defer srv.Close()
	base := srv.URL + "/api/tls/hosts"
	cert, key := genCert(t, "idlibookfair.com")

	t.Run("unauth → 401", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("empty list", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "tok", nil)
		b := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
		}
		if !strings.Contains(b, `"hosts":[]`) {
			t.Errorf("expected empty hosts; got: %s", b)
		}
	})

	t.Run("put valid → 200", func(t *testing.T) {
		resp := doJSON(t, http.MethodPut, base+"/idlibookfair.com", "tok", map[string]any{"cert": cert, "key": key})
		b := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
		}
		var got hostCertResponse
		if err := json.Unmarshal([]byte(b), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Host != "idlibookfair.com" || got.CertSecret == "" || got.CertHash == "" {
			t.Errorf("unexpected response: %+v", got)
		}
		if rec.refreshes != 1 {
			t.Errorf("refreshes = %d, want 1", rec.refreshes)
		}
	})

	t.Run("list shows host", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "tok", nil)
		b := readBody(t, resp)
		if !strings.Contains(b, "idlibookfair.com") {
			t.Errorf("host missing from list; got: %s", b)
		}
	})

	t.Run("put bad PEM → 400", func(t *testing.T) {
		resp := doJSON(t, http.MethodPut, base+"/other.example.com", "tok", map[string]any{"cert": "not-a-pem", "key": key})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, readBody(t, resp))
		}
	})

	t.Run("put missing key → 400", func(t *testing.T) {
		resp := doJSON(t, http.MethodPut, base+"/other.example.com", "tok", map[string]any{"cert": cert})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, readBody(t, resp))
		}
	})

	t.Run("delete → 204 + refresh", func(t *testing.T) {
		before := rec.refreshes
		resp := doJSON(t, http.MethodDelete, base+"/idlibookfair.com", "tok", nil)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body: %s", resp.StatusCode, readBody(t, resp))
		}
		if rec.refreshes != before+1 {
			t.Errorf("refreshes = %d, want %d", rec.refreshes, before+1)
		}
	})

	t.Run("delete absent → 404", func(t *testing.T) {
		resp := doJSON(t, http.MethodDelete, base+"/gone.example.com", "tok", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body: %s", resp.StatusCode, readBody(t, resp))
		}
	})

	t.Run("traversal host → 400", func(t *testing.T) {
		resp := doJSON(t, http.MethodPut, base+"/..%2F..%2Fevil", "tok", map[string]any{"cert": cert, "key": key})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, readBody(t, resp))
		}
	})
}

func TestHostCertAPI_ListExcludesClusterDomain(t *testing.T) {
	st := openStore(t)
	if err := st.SetSetting(context.Background(), "domain", "nextrum-sy.com"); err != nil {
		t.Fatalf("set setting: %v", err)
	}
	now := time.Now().UTC()
	if err := st.PutSiteCert(context.Background(), store.SiteCertRow{
		Domain: "nextrum-sy.com", CertSecret: "cert_v041", KeySecret: "key_v041",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed main: %v", err)
	}
	if err := st.PutSiteCert(context.Background(), store.SiteCertRow{
		Domain: "idlebbookfair.com", CertSecret: "hostcert-idlebbookfair-com_v001", KeySecret: "hostkey-..._v001",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed host: %v", err)
	}
	rec := &fakeService{store: st, main: "nextrum-sy.com"}
	srv := httptest.NewServer(mountRoutes(&HTTP{Svc: rec}))
	defer srv.Close()

	resp := doJSON(t, http.MethodGet, srv.URL+"/api/tls/hosts", "tok", nil)
	b := readBody(t, resp)
	if strings.Contains(b, "nextrum-sy.com") {
		t.Errorf("cluster domain must be excluded from host list; got: %s", b)
	}
	if !strings.Contains(b, "idlebbookfair.com") {
		t.Errorf("host missing from list; got: %s", b)
	}
}

func TestHostCertAPI_ApplyError_ReportsError(t *testing.T) {
	st := openStore(t)
	rec := &fakeService{store: st, main: "nextrum-sy.com", fail: errors.New("simulated refresh failure")}
	srv := httptest.NewServer(mountRoutes(&HTTP{Svc: rec}))
	defer srv.Close()
	cert, key := genCert(t, "fail.example.com")

	resp := doJSON(t, http.MethodPut, srv.URL+"/api/tls/hosts/fail.example.com", "tok", map[string]any{"cert": cert, "key": key})
	b := readBody(t, resp)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", resp.StatusCode, b)
	}
	if _, err := st.GetSiteCert(context.Background(), "fail.example.com"); err == nil {
		t.Errorf("row must not be persisted on apply error")
	}
}
