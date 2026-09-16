package tlscerts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

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

func TestValidate(t *testing.T) {
	for _, ok := range []string{"example.com", "www.example.com", "*.example.com", "a-b.example.com"} {
		if err := Validate(ok); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "..", "../evil", "a/b", "a\\b", "in valid.com", "-x.com"} {
		if err := Validate(bad); err == nil {
			t.Errorf("Validate(%q) = nil, want error", bad)
		}
	}
}

func TestParseAndCheck(t *testing.T) {
	certPEM, keyPEM := testCert(t, "idlibookfair.com", "www.idlibookfair.com")
	info, err := ParseAndCheck(certPEM, keyPEM, "idlibookfair.com")
	if err != nil {
		t.Fatalf("ParseAndCheck: %v", err)
	}
	if len(info.SANs) != 2 {
		t.Errorf("SANs = %v, want 2", info.SANs)
	}
	if !info.NotAfter.After(info.NotBefore) {
		t.Errorf("NotAfter %v should be after NotBefore %v", info.NotAfter, info.NotBefore)
	}
}

func TestParseAndCheck_Errors(t *testing.T) {
	certPEM, keyPEM := testCert(t, "idlibookfair.com")

	if _, err := ParseAndCheck("not-a-cert", keyPEM, "idlibookfair.com"); err == nil {
		t.Error("bad cert should error")
	}
	certA, _ := testCert(t, "a.example.com")
	_, keyB := testCert(t, "b.example.com")
	if _, err := ParseAndCheck(certA, keyB, "a.example.com"); err == nil {
		t.Error("mismatched key should error")
	}
	if _, err := ParseAndCheck(certPEM, keyPEM, "other.example.com"); err == nil || !strings.Contains(err.Error(), "does not cover") {
		t.Errorf("host-not-covered error = %v", err)
	}
	if _, err := ParseAndCheck("", keyPEM, "idlibookfair.com"); err == nil {
		t.Error("empty cert should error")
	}
}

func TestParseAndCheck_WildcardCoversSubdomain(t *testing.T) {
	certPEM, keyPEM := testCert(t, "*.example.com")
	if _, err := ParseAndCheck(certPEM, keyPEM, "api.example.com"); err != nil {
		t.Fatalf("wildcard cert should cover subdomain: %v", err)
	}
}
