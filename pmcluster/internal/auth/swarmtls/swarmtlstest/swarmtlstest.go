// Package swarmtlstest mints the certificates a Docker Swarm would have issued, so
// the swarm-TLS checks can be exercised without a swarm, a Docker daemon or a
// network. It mirrors the shape Swarm uses exactly, because a helper that is
// "close enough" would test a different rule:
//
//   - one root CA;
//   - a per-node certificate whose ORGANISATION is the swarm (Raft) ID;
//   - whose COMMON NAME is the node ID;
//   - whose ORGANISATIONAL UNIT names the role (swarm-manager / swarm-worker);
//   - whose SANs are DNS entries only (Docker puts no IP SAN on them, which is
//     why the client replaces the hostname check instead of disabling it).
package swarmtlstest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CA is a test swarm root.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	PEM  []byte
	// Pool is the root, ready to verify chains.
	Pool *x509.CertPool
}

// New mints a root CA.
func New(tb testing.TB) *CA {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatalf("swarmtlstest: generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "swarm-ca"},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().Add(20 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		tb.Fatalf("swarmtlstest: create CA: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		tb.Fatalf("swarmtlstest: parse CA: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		tb.Fatal("swarmtlstest: could not add the CA to a pool")
	}
	return &CA{Cert: cert, Key: key, PEM: pemBytes, Pool: pool}
}

// NodeCert is one issued node certificate, as a loaded keypair and as PEM bytes.
type NodeCert struct {
	Pair    tls.Certificate
	CertPEM []byte
	KeyPEM  []byte
}

// Issue mints a node certificate: O = swarmID, CN = nodeID, OU = role.
func (c *CA) Issue(tb testing.TB, swarmID, nodeID, role string, notBefore, notAfter time.Time) NodeCert {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatalf("swarmtlstest: generate node key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName:         nodeID,
			Organization:       []string{swarmID},
			OrganizationalUnit: []string{role},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		// Docker's node certificates carry DNS names only — no IP SAN. Any test
		// that dials by address must therefore replace the hostname check.
		DNSNames: []string{role, nodeID},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, &key.PublicKey, c.Key)
	if err != nil {
		tb.Fatalf("swarmtlstest: create node certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		tb.Fatalf("swarmtlstest: marshal node key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		tb.Fatalf("swarmtlstest: load node keypair: %v", err)
	}
	return NodeCert{Pair: pair, CertPEM: certPEM, KeyPEM: keyPEM}
}

// WriteCA writes the root CA into dir and returns its path (the --CAPaths shape).
func (c *CA) WriteCA(tb testing.TB, dir string) string {
	tb.Helper()
	path := filepath.Join(dir, "swarm-root-ca.crt")
	if err := os.WriteFile(path, c.PEM, 0o644); err != nil {
		tb.Fatalf("swarmtlstest: write CA: %v", err)
	}
	return path
}

// WriteNode writes a node certificate and key into dir (the
// /var/lib/docker/swarm/certificates layout) and returns their paths.
func WriteNode(tb testing.TB, dir, prefix string, n NodeCert) (certFile, keyFile string) {
	tb.Helper()
	certFile = filepath.Join(dir, prefix+"-node.crt")
	keyFile = filepath.Join(dir, prefix+"-node.key")
	if err := os.WriteFile(certFile, n.CertPEM, 0o644); err != nil {
		tb.Fatalf("swarmtlstest: write node cert: %v", err)
	}
	if err := os.WriteFile(keyFile, n.KeyPEM, 0o600); err != nil {
		tb.Fatalf("swarmtlstest: write node key: %v", err)
	}
	return certFile, keyFile
}
