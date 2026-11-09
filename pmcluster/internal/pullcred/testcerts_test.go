package pullcred

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"
)

// Test certificates, shaped like the ones Docker issues on `swarm join`:
// a CA (CN=swarm-ca), and node certificates whose O is the swarm ID and whose
// CN is the node ID. They are generated per test — no fixture is reused, so a
// test can hand out a certificate from a different CA or an expired one without
// disturbing another test.

type testAuthority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestAuthority(t *testing.T, cn string) *testAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	return &testAuthority{
		cert: cert,
		key:  key,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// issue mints a leaf certificate. org is the swarm ID, ou the role, cn the
// node ID — exactly Docker's subject layout.
func (a *testAuthority) issue(t *testing.T, org, ou, cn string, notBefore, notAfter time.Time, usages ...x509.ExtKeyUsage) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	subject := pkix.Name{CommonName: cn}
	if org != "" {
		subject.Organization = []string{org}
	}
	if ou != "" {
		subject.OrganizationalUnit = []string{ou}
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               subject,
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           usages,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatalf("create leaf certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf certificate: %v", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        cert,
	}, cert
}

// pkixNameForTest builds a certificate subject without a key, for the tests
// that call Verifier directly rather than through a handshake.
func pkixNameForTest(org, cn string) pkix.Name {
	return pkix.Name{Organization: []string{org}, CommonName: cn}
}

// newNodeCert is the certificate a healthy swarm node presents.
func (a *testAuthority) newNodeCert(t *testing.T, swarmID, nodeID string) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	return a.issue(t, swarmID, "swarm-manager", nodeID,
		time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour),
		x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth)
}

// handshake runs a real TLS handshake over an in-memory pipe and returns both
// sides' errors. The client skips verifying the server: what these tests are
// about is the SERVER's verification of the client certificate, and a real
// handshake is the only place that whole path (chain building, expiry, the
// swarm membership rule) actually runs.
func handshake(t *testing.T, serverCfg, clientCfg *tls.Config) (serverErr, clientErr error) {
	t.Helper()
	srvConn, cliConn := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = srvConn.SetDeadline(deadline)
	_ = cliConn.SetDeadline(deadline)

	done := make(chan error, 1)
	go func() {
		err := tls.Server(srvConn, serverCfg).Handshake()
		_ = srvConn.Close()
		done <- err
	}()
	clientErr = tls.Client(cliConn, clientCfg).Handshake()
	_ = cliConn.Close()
	return <-done, clientErr
}

// clientConfig presents cert and skips verifying the server (see handshake).
func clientConfig(cert *tls.Certificate) *tls.Config {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
	}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	return cfg
}
