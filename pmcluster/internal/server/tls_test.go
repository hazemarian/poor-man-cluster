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
	"testing"
	"time"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster/tlscerts"
)

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

func newTLSServer(t *testing.T) (*httptest.Server, *int, *tlscerts.Manager) {
	t.Helper()
	mgr := tlscerts.New(t.TempDir())
	refreshes := 0
	srv := httptest.NewServer(New(Deps{
		Lookup: &fakeLookup{users: map[string]*auth.User{"tok": {Name: "admin"}}},
		HostCerts: &HostCertService{
			Manager: mgr,
			Refresh: func(context.Context) error { refreshes++; return nil },
		},
	}))
	t.Cleanup(srv.Close)
	return srv, &refreshes, mgr
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
	srv, refreshes, _ := newTLSServer(t)
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
			Hosts []tlscerts.HostCert `json:"hosts"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if len(out.Hosts) != 0 {
			t.Errorf("hosts = %d, want 0", len(out.Hosts))
		}
	})

	t.Run("put valid → 200 + refresh", func(t *testing.T) {
		resp := doJSON(t, http.MethodPut, base+"/idlibookfair.com", "tok",
			map[string]string{"cert": cert, "key": key})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b := new(bytes.Buffer)
			_, _ = b.ReadFrom(resp.Body)
			t.Fatalf("status = %d, body=%s", resp.StatusCode, b.String())
		}
		if *refreshes != 1 {
			t.Errorf("refreshes = %d, want 1", *refreshes)
		}
	})

	t.Run("list shows host", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "tok", nil)
		defer resp.Body.Close()
		var out struct {
			Hosts []tlscerts.HostCert `json:"hosts"`
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

// TestHostCertAPI_RefreshFails_Reports503 verifies a failed refresh surfaces
// as 503 (the on-disk write is kept, so a retry re-triggers it).
func TestHostCertAPI_RefreshFails_Reports503(t *testing.T) {
	mgr := tlscerts.New(t.TempDir())
	srv := httptest.NewServer(New(Deps{
		Lookup: &fakeLookup{users: map[string]*auth.User{"tok": {Name: "admin"}}},
		HostCerts: &HostCertService{
			Manager: mgr,
			Refresh: func(context.Context) error { return errors.New("simulated refresh failure") },
		},
	}))
	t.Cleanup(srv.Close)
	cert, key := genCert(t, "fail.example.com")
	resp := doJSON(t, http.MethodPut, srv.URL+"/api/tls/hosts/fail.example.com", "tok",
		map[string]string{"cert": cert, "key": key})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	// Cert remains on disk.
	list, err := mgr.List(context.Background())
	if err != nil || len(list) != 1 {
		t.Errorf("cert should persist after failed refresh; list=%+v err=%v", list, err)
	}
}
