package pullcred

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	testSwarmID   = "lm03ez1is3mlvt8zevoc2lfis"
	testNodeID    = "n9xllyznqu52d6me0r6oeqhvv"
	testOtherNode = "fhl4j2gy4exyrotyky3g2oz6f"
)

// newNodeIDs is the node list the listener reads: this swarm's live members.
func newNodeIDs(ids ...string) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) { return ids, nil }
}

func newVerifier(ids ...string) *Verifier {
	return &Verifier{SwarmID: testSwarmID, NodeIDs: newNodeIDs(ids...)}
}

// TestVerifierAcceptsANodeOfThisSwarm is the accept case, through a real TLS
// handshake with the swarm root CA as the client CA.
func TestVerifierAcceptsANodeOfThisSwarm(t *testing.T) {
	ca := newTestAuthority(t, "swarm-ca")
	serverCert, err := SelfSignedServerCert("127.0.0.1")
	if err != nil {
		t.Fatalf("server certificate: %v", err)
	}
	cfg, err := newVerifier(testNodeID, testOtherNode).TLSConfig(ca.pem, serverCert)
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	leafCert, _ := ca.newNodeCert(t, testSwarmID, testNodeID)

	srvErr, cliErr := handshake(t, cfg, clientConfig(&leafCert))
	if srvErr != nil {
		t.Fatalf("server rejected a node certificate of this swarm: %v", srvErr)
	}
	if cliErr != nil {
		t.Fatalf("client handshake failed: %v", cliErr)
	}
}

// TestVerifierRejectsAForeignCA: a well-formed certificate (right O, right CN)
// signed by a CA that is not this swarm's is refused by the chain check.
func TestVerifierRejectsAForeignCA(t *testing.T) {
	ca := newTestAuthority(t, "swarm-ca")
	foreign := newTestAuthority(t, "swarm-ca") // same subject, different key
	serverCert, err := SelfSignedServerCert("127.0.0.1")
	if err != nil {
		t.Fatalf("server certificate: %v", err)
	}
	cfg, err := newVerifier(testNodeID).TLSConfig(ca.pem, serverCert)
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	leafCert, _ := foreign.newNodeCert(t, testSwarmID, testNodeID)

	srvErr, _ := handshake(t, cfg, clientConfig(&leafCert))
	if srvErr == nil {
		t.Fatal("a certificate signed by a foreign CA was accepted")
	}
	var unknown x509.UnknownAuthorityError
	if !errors.As(srvErr, &unknown) && !strings.Contains(srvErr.Error(), "unknown authority") {
		t.Fatalf("rejection = %v, want a chain failure naming the unknown authority", srvErr)
	}
}

// TestVerifierRejectsAWrongOrganization: the certificate chains to this swarm's
// CA but names another swarm in O — a certificate that should never have been
// usable here.
func TestVerifierRejectsAWrongOrganization(t *testing.T) {
	ca := newTestAuthority(t, "swarm-ca")
	serverCert, err := SelfSignedServerCert("127.0.0.1")
	if err != nil {
		t.Fatalf("server certificate: %v", err)
	}
	cfg, err := newVerifier(testNodeID).TLSConfig(ca.pem, serverCert)
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	leafCert, _ := ca.newNodeCert(t, "other-swarm-id", testNodeID)

	srvErr, _ := handshake(t, cfg, clientConfig(&leafCert))
	if srvErr == nil {
		t.Fatal("a certificate whose O is another swarm was accepted")
	}
	if !errors.Is(srvErr, ErrForeignSwarm) {
		t.Fatalf("rejection = %v, want ErrForeignSwarm", srvErr)
	}
}

// TestVerifierRejectsANodeThatIsNotInTheSwarm: a genuine swarm certificate for
// a node that left (or was never a member) is refused by the node list.
func TestVerifierRejectsANodeThatIsNotInTheSwarm(t *testing.T) {
	ca := newTestAuthority(t, "swarm-ca")
	serverCert, err := SelfSignedServerCert("127.0.0.1")
	if err != nil {
		t.Fatalf("server certificate: %v", err)
	}
	cfg, err := newVerifier(testNodeID).TLSConfig(ca.pem, serverCert)
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	leafCert, _ := ca.newNodeCert(t, testSwarmID, "a-node-that-left")

	srvErr, _ := handshake(t, cfg, clientConfig(&leafCert))
	if srvErr == nil {
		t.Fatal("a certificate for a node outside the node list was accepted")
	}
	if !errors.Is(srvErr, ErrUnknownNode) {
		t.Fatalf("rejection = %v, want ErrUnknownNode", srvErr)
	}
	if !strings.Contains(srvErr.Error(), "a-node-that-left") {
		t.Errorf("rejection %q does not name the CN it refused", srvErr)
	}
}

// TestVerifierRejectsAnExpiredCertificate: swarm certificates rotate; an old
// one is not a credential any more.
func TestVerifierRejectsAnExpiredCertificate(t *testing.T) {
	ca := newTestAuthority(t, "swarm-ca")
	serverCert, err := SelfSignedServerCert("127.0.0.1")
	if err != nil {
		t.Fatalf("server certificate: %v", err)
	}
	cfg, err := newVerifier(testNodeID).TLSConfig(ca.pem, serverCert)
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	expired, _ := ca.issue(t, testSwarmID, "swarm-worker", testNodeID,
		time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour),
		x509.ExtKeyUsageClientAuth)

	srvErr, _ := handshake(t, cfg, clientConfig(&expired))
	if srvErr == nil {
		t.Fatal("an expired certificate was accepted")
	}
	if !errors.Is(srvErr, ErrExpired) && !strings.Contains(strings.ToLower(srvErr.Error()), "expired") {
		t.Fatalf("rejection = %v, want an expiry failure", srvErr)
	}
}

// TestVerifierRejectsAMissingCertificate: nothing presented, nothing served.
func TestVerifierRejectsAMissingCertificate(t *testing.T) {
	ca := newTestAuthority(t, "swarm-ca")
	serverCert, err := SelfSignedServerCert("127.0.0.1")
	if err != nil {
		t.Fatalf("server certificate: %v", err)
	}
	cfg, err := newVerifier(testNodeID).TLSConfig(ca.pem, serverCert)
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	srvErr, _ := handshake(t, cfg, clientConfig(nil))
	if srvErr == nil {
		t.Fatal("a connection with no client certificate was accepted")
	}
}

// TestVerifierFailsClosed covers the two configuration states that must refuse
// everything rather than accept a certificate nobody checked against.
func TestVerifierFailsClosed(t *testing.T) {
	leaf := &x509.Certificate{
		Subject:   pkixNameForTest(testSwarmID, testNodeID),
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(time.Hour),
	}

	if err := (&Verifier{NodeIDs: newNodeIDs(testNodeID)}).Verify(context.Background(), leaf); !errors.Is(err, ErrNoSwarmID) {
		t.Errorf("no swarm ID: %v, want ErrNoSwarmID", err)
	}
	if err := (&Verifier{SwarmID: testSwarmID}).Verify(context.Background(), leaf); !errors.Is(err, ErrUnknownNode) {
		t.Errorf("no node list: %v, want ErrUnknownNode", err)
	}
	if err := (&Verifier{SwarmID: testSwarmID, NodeIDs: func(context.Context) ([]string, error) {
		return nil, errors.New("docker unreachable")
	}}).Verify(context.Background(), leaf); err == nil || !strings.Contains(err.Error(), "docker unreachable") {
		t.Errorf("node list failure: %v, want the lookup error to surface", err)
	}
	if err := newVerifier(testNodeID).Verify(context.Background(), nil); !errors.Is(err, ErrNoPeerCertificate) {
		t.Errorf("nil certificate: %v, want ErrNoPeerCertificate", err)
	}
}

// TestTLSConfigRejectsANonPEMCA: a listener that cannot load the root CA must
// not start with an empty pool (which would reject everything, or worse, be
// silently replaced by the system pool).
func TestTLSConfigRejectsANonPEMCA(t *testing.T) {
	serverCert, err := SelfSignedServerCert("127.0.0.1")
	if err != nil {
		t.Fatalf("server certificate: %v", err)
	}
	if _, err := newVerifier(testNodeID).TLSConfig([]byte("not a certificate"), serverCert); err == nil {
		t.Fatal("TLSConfig accepted a CA blob with no certificate in it")
	}
}

// TestTLSConfigRequiresAClientCertificate pins the one setting that makes the
// route's authentication real.
func TestTLSConfigRequiresAClientCertificate(t *testing.T) {
	ca := newTestAuthority(t, "swarm-ca")
	serverCert, err := SelfSignedServerCert("127.0.0.1")
	if err != nil {
		t.Fatalf("server certificate: %v", err)
	}
	cfg, err := newVerifier(testNodeID).TLSConfig(ca.pem, serverCert)
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.VerifyConnection == nil {
		t.Error("VerifyConnection is nil: the swarm membership rule would never run")
	}
}
