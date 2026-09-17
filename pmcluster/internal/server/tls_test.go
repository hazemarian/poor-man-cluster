package server

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
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster/tlscerts"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// openServerStore opens a fresh store in a temp dir for one test.
func openServerStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func genCert(t *testing.T, host string) (certPEM, keyPEM string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	kder, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
	return certPEM, keyPEM
}

// newHostTLSServer builds a server with a real store + a recording Apply that
// persists rows via st.PutSiteCert (mirroring cluster.ApplyCert) and a
// Remove that deletes rows + secrets via st.DeleteSiteCert.
func newHostTLSServer(t *testing.T) (*httptest.Server, *int, *store.Store) {
	t.Helper()
	st := openServerStore(t)
	refreshes := 0
	applyCalls := 0
	srv := httptest.NewServer(New(Deps{
		Lookup:    &fakeLookup{users: map[string]*auth.User{"tok": {Name: "admin"}}},
		Store:     st,
		HostCerts: &HostCertService{Svc: &hostTLSSvc{store: st, refreshes: &refreshes, applyCalls: &applyCalls}},
	}))
	t.Cleanup(srv.Close)
	return srv, &refreshes, st
}

// hostTLSSvc implements service.TLSService for the host-cert tests with the
// same recording semantics as the old Apply/Remove closures.
type hostTLSSvc struct {
	store      *store.Store
	refreshes  *int
	applyCalls *int
	fail       error
}

func (h *hostTLSSvc) SiteCert(ctx context.Context, domain, certPEM, keyPEM string) (*store.SiteCertRow, error) {
	return nil, nil
}

func (h *hostTLSSvc) ApplyHostCert(ctx context.Context, host, certPEM, keyPEM string, _ bool) (*store.SiteCertRow, error) {
	if h.fail != nil {
		return nil, h.fail
	}
	if h.applyCalls != nil {
		*h.applyCalls++
	}
	if err := tlscerts.Validate(host); err != nil {
		return nil, err
	}
	info, err := tlscerts.ParseAndCheck(certPEM, keyPEM, host)
	if err != nil {
		return nil, err
	}
	if h.refreshes != nil {
		*h.refreshes++
	}
	now := time.Now().UTC()
	row := store.SiteCertRow{
		Domain:     host,
		CertSecret: "hostcert-" + host + "_v001",
		KeySecret:  "hostkey-" + host + "_v001",
		NotBefore:  info.NotBefore,
		NotAfter:   info.NotAfter,
		SANs:       info.SANs,
		CertHash:   store.ConfigHash(certPEM),
		KeyHash:    store.ConfigHash(keyPEM),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := h.store.PutSiteCert(ctx, row); err != nil {
		return nil, err
	}
	return &row, nil
}

func (h *hostTLSSvc) RemoveHostCert(ctx context.Context, host string, _ bool) error {
	if h.refreshes != nil {
		*h.refreshes++
	}
	if _, err := h.store.GetSiteCert(ctx, host); err != nil {
		return err
	}
	return h.store.DeleteSiteCert(ctx, host)
}

func (h *hostTLSSvc) List(ctx context.Context) ([]store.SiteCertRow, error) {
	rows, err := h.store.ListSiteCerts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]store.SiteCertRow, 0, len(rows))
	return append(out, rows...), nil
}

func (h *hostTLSSvc) GetSiteCert(ctx context.Context, domain string) (*store.SiteCertRow, error) {
	row, err := h.store.GetSiteCert(ctx, domain)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (h *hostTLSSvc) MainDomain(ctx context.Context) (string, error) {
	return h.store.GetSettingDefault(ctx, "domain", ""), nil
}

func doJSON(t *testing.T, method, url, token string, body any) *http.Response {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, url, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func TestHostCertAPI_CRUDAndAuth(t *testing.T) {
	srv, refreshes, st := newHostTLSServer(t)
	base := srv.URL + "/api/tls/hosts"
	cert, key := genCert(t, "idlibookfair.com")

	t.Run("unauth → 401", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("empty list", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "tok", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var out struct {
			Hosts []hostCertResponse `json:"hosts"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if len(out.Hosts) != 0 {
			t.Errorf("hosts = %d, want 0", len(out.Hosts))
		}
	})

	t.Run("put valid → 200 + row persisted", func(t *testing.T) {
		resp := doJSON(t, http.MethodPut, base+"/idlibookfair.com", "tok",
			map[string]string{"cert": cert, "key": key})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b := new(bytes.Buffer)
			_, _ = b.ReadFrom(resp.Body)
			t.Fatalf("status = %d, body=%s", resp.StatusCode, b.String())
		}
		var out hostCertResponse
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if out.Host != "idlibookfair.com" || out.CertSecret == "" || out.KeySecret == "" || out.CertHash == "" {
			t.Errorf("response = %+v, want host+secrets+hash", out)
		}
		if *refreshes != 1 {
			t.Errorf("refreshes = %d, want 1", *refreshes)
		}
		if _, err := st.GetSiteCert(context.Background(), "idlibookfair.com"); err != nil {
			t.Errorf("row not persisted: %v", err)
		}
	})

	t.Run("list shows host", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "tok", nil)
		defer resp.Body.Close()
		var out struct {
			Hosts []hostCertResponse `json:"hosts"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if len(out.Hosts) != 1 || out.Hosts[0].Host != "idlibookfair.com" {
			t.Errorf("hosts = %+v, want idlibookfair.com", out.Hosts)
		}
	})

	t.Run("put bad PEM → 400", func(t *testing.T) {
		resp := doJSON(t, http.MethodPut, base+"/idlibookfair.com", "tok",
			map[string]string{"cert": "nope", "key": "nope"})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("put missing key → 400", func(t *testing.T) {
		resp := doJSON(t, http.MethodPut, base+"/idlibookfair.com", "tok",
			map[string]string{"cert": cert})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("delete → 204 + refresh", func(t *testing.T) {
		before := *refreshes
		resp := doJSON(t, http.MethodDelete, base+"/idlibookfair.com", "tok", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", resp.StatusCode)
		}
		if *refreshes != before+1 {
			t.Errorf("refreshes = %d, want %d", *refreshes, before+1)
		}
	})

	t.Run("delete absent → 404", func(t *testing.T) {
		resp := doJSON(t, http.MethodDelete, base+"/idlibookfair.com", "tok", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("put traversal host → 400", func(t *testing.T) {
		resp := doJSON(t, http.MethodPut, base+"/..%2F..%2Fevil", "tok",
			map[string]string{"cert": cert, "key": key})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})
}

// TestHostCertAPI_ListExcludesClusterDomain verifies the list endpoint omits
// the cluster's own-domain row (that one is served by /api/tls/site).
func TestHostCertAPI_ListExcludesClusterDomain(t *testing.T) {
	st := openServerStore(t)
	ctx := context.Background()
	_ = st.SetSetting(ctx, "domain", "nextrum-sy.com")
	now := time.Now().UTC()
	if err := st.PutSiteCert(ctx, store.SiteCertRow{
		Domain: "nextrum-sy.com", CertSecret: "cert_v001", KeySecret: "key_v001",
		NotBefore: now, NotAfter: now.Add(24 * time.Hour), SANs: []string{"nextrum-sy.com"},
		CertHash: "a", KeyHash: "b", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed main row: %v", err)
	}
	if err := st.PutSiteCert(ctx, store.SiteCertRow{
		Domain: "idlibookfair.com", CertSecret: "hostcert-idlibookfair-com_v001", KeySecret: "hostkey-idlibookfair-com_v001",
		NotBefore: now, NotAfter: now.Add(24 * time.Hour), SANs: []string{"idlibookfair.com"},
		CertHash: "c", KeyHash: "d", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed host row: %v", err)
	}

	srv := httptest.NewServer(New(Deps{
		Lookup:    &fakeLookup{users: map[string]*auth.User{"tok": {Name: "admin"}}},
		Store:     st,
		HostCerts: &HostCertService{Svc: &hostTLSSvc{store: st}},
	}))
	t.Cleanup(srv.Close)

	resp := doJSON(t, http.MethodGet, srv.URL+"/api/tls/hosts", "tok", nil)
	defer resp.Body.Close()
	var out struct {
		Hosts []hostCertResponse `json:"hosts"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Hosts) != 1 || out.Hosts[0].Host != "idlibookfair.com" {
		t.Errorf("hosts = %+v, want only idlibookfair.com (main row excluded)", out.Hosts)
	}
}

// TestHostCertAPI_RefreshFails_Reports503 verifies an Apply failure surfaces
// as an error response (the row is not persisted).
func TestHostCertAPI_ApplyError_ReportsError(t *testing.T) {
	st := openServerStore(t)
	srv := httptest.NewServer(New(Deps{
		Lookup:    &fakeLookup{users: map[string]*auth.User{"tok": {Name: "admin"}}},
		Store:     st,
		HostCerts: &HostCertService{Svc: &hostTLSSvc{store: st, fail: errors.New("simulated refresh failure")}},
	}))
	t.Cleanup(srv.Close)
	cert, key := genCert(t, "fail.example.com")
	resp := doJSON(t, http.MethodPut, srv.URL+"/api/tls/hosts/fail.example.com", "tok",
		map[string]string{"cert": cert, "key": key})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}

	if _, err := st.GetSiteCert(context.Background(), "fail.example.com"); err == nil {
		t.Errorf("row should not persist when Apply fails")
	}
}
