package tlscerts

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testCert generates a self-signed ECDSA cert/key for the given hosts (DNS
// names) and returns the PEM strings.
func testCert(t *testing.T, hosts ...string) (certPEM, keyPEM string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: hosts[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     hosts,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}

func newTestMgr(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	return New(dir), filepath.Join(dir, Subdir)
}

func TestPutAndList(t *testing.T) {
	m, hostsDir := newTestMgr(t)
	certPEM, keyPEM := testCert(t, "idlibookfair.com", "www.idlibookfair.com")

	hc, err := m.Put(context.Background(), "idlibookfair.com", certPEM, keyPEM)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if hc.Host != "idlibookfair.com" {
		t.Errorf("Host = %q", hc.Host)
	}
	if len(hc.SANs) != 2 {
		t.Errorf("SANs = %v, want 2", hc.SANs)
	}
	if _, err := os.Stat(hostsDir); err != nil {
		t.Errorf("hosts dir should exist: %v", err)
	}

	// Files written with 0600 perms.
	for _, p := range []string{hc.CertFile, hc.KeyFile} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s perms = %o, want 600", p, perm)
		}
	}

	list, err := m.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List len = %d, want 1", len(list))
	}
	if list[0].Host != "idlibookfair.com" {
		t.Errorf("List[0].Host = %q", list[0].Host)
	}
}

func TestPut_InvalidPEM(t *testing.T) {
	m, _ := newTestMgr(t)
	_, keyPEM := testCert(t, "idlibookfair.com")
	if _, err := m.Put(context.Background(), "idlibookfair.com", "not-a-cert", keyPEM); err == nil {
		t.Fatal("Put with bad cert should error")
	}
}

func TestPut_KeyCertMismatch(t *testing.T) {
	m, _ := newTestMgr(t)
	certA, _ := testCert(t, "a.example.com")
	_, keyB := testCert(t, "b.example.com")
	if _, err := m.Put(context.Background(), "a.example.com", certA, keyB); err == nil {
		t.Fatal("Put with mismatched key should error")
	}
}

func TestPut_HostNotCovered(t *testing.T) {
	m, _ := newTestMgr(t)
	certPEM, keyPEM := testCert(t, "other.example.com")
	if _, err := m.Put(context.Background(), "idlibookfair.com", certPEM, keyPEM); err == nil {
		t.Fatal("Put with cert not covering host should error")
	}
}

func TestPut_RejectsPathTraversal(t *testing.T) {
	m, _ := newTestMgr(t)
	certPEM, keyPEM := testCert(t, "idlibookfair.com")
	for _, bad := range []string{"../evil", "a/b", "a\\b", "..", ""} {
		if _, err := m.Put(context.Background(), bad, certPEM, keyPEM); err == nil {
			t.Errorf("Put host %q should error", bad)
		}
	}
}

func TestPut_OverwritesExisting(t *testing.T) {
	m, _ := newTestMgr(t)
	certA, keyA := testCert(t, "idlibookfair.com")
	certB, keyB := testCert(t, "idlibookfair.com", "www.idlibookfair.com")
	if _, err := m.Put(context.Background(), "idlibookfair.com", certA, keyA); err != nil {
		t.Fatalf("Put A: %v", err)
	}
	hc, err := m.Put(context.Background(), "idlibookfair.com", certB, keyB)
	if err != nil {
		t.Fatalf("Put B: %v", err)
	}
	if len(hc.SANs) != 2 {
		t.Errorf("after overwrite SANs = %v, want 2", hc.SANs)
	}
	list, _ := m.List(context.Background())
	if len(list) != 1 {
		t.Errorf("List len = %d, want 1 (overwrite not duplicate)", len(list))
	}
}

func TestPut_WildcardCertCoversSubdomain(t *testing.T) {
	m, _ := newTestMgr(t)
	certPEM, keyPEM := testCert(t, "*.example.com")
	if _, err := m.Put(context.Background(), "api.example.com", certPEM, keyPEM); err != nil {
		t.Fatalf("wildcard cert should cover subdomain: %v", err)
	}
}

func TestRemove(t *testing.T) {
	m, _ := newTestMgr(t)
	certPEM, keyPEM := testCert(t, "idlibookfair.com")
	if _, err := m.Put(context.Background(), "idlibookfair.com", certPEM, keyPEM); err != nil {
		t.Fatalf("Put: %v", err)
	}

	removed, err := m.Remove(context.Background(), "idlibookfair.com")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Error("Remove should report removed=true for existing host")
	}
	list, _ := m.List(context.Background())
	if len(list) != 0 {
		t.Errorf("List len = %d after remove, want 0", len(list))
	}

	// Removing again is a no-op.
	removed, err = m.Remove(context.Background(), "idlibookfair.com")
	if err != nil {
		t.Fatalf("Remove (again): %v", err)
	}
	if removed {
		t.Error("Remove of absent host should report removed=false")
	}
}

func TestList_Empty(t *testing.T) {
	m, _ := newTestMgr(t)
	list, err := m.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("List len = %d, want 0", len(list))
	}
}

func TestHostsDir(t *testing.T) {
	if got := HostsDir("/a/b"); got != filepath.Join("/a/b", "hosts") {
		t.Errorf("HostsDir = %q", got)
	}
}

func TestNormalizeLowercasesHost(t *testing.T) {
	m, _ := newTestMgr(t)
	certPEM, keyPEM := testCert(t, "IDLIBOOKFAIR.COM")
	hc, err := m.Put(context.Background(), "IDLIBOOKFAIR.COM", certPEM, keyPEM)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if hc.Host != "idlibookfair.com" {
		t.Errorf("Host = %q, want lowercased", hc.Host)
	}
	if !strings.Contains(hc.CertFile, "idlibookfair.com") {
		t.Errorf("CertFile %q should use lowercased host dir", hc.CertFile)
	}
}
