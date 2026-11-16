package swarmtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth/swarmtls/swarmtlstest"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// fakeNodes is the live node list: the CN allowlist and the revocation path.
type fakeNodes struct {
	nodes []runtime.Node
	err   error
}

func (f fakeNodes) NodeList(context.Context) ([]runtime.Node, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.nodes, nil
}

// serverTLS is the listener configuration Serve uses, without binding a port.
func serverTLS(ca *swarmtlstest.CA, serverPair tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{serverPair},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.Pool,
		MinVersion:   tls.VersionTLS12,
	}
}

// clientTLS dials a test server: the CA pool is the check, and the hostname check
// is replaced (Docker's node certificates carry no IP SAN).
func clientTLS(ca *swarmtlstest.CA, pair *tls.Certificate) *tls.Config {
	cfg := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // the CA pool below IS the check
		RootCAs:            ca.Pool,
		MinVersion:         tls.VersionTLS12,
	}
	if pair != nil {
		cfg.Certificates = []tls.Certificate{*pair}
	}
	return cfg
}

// TestVerifierRules covers the four verification rules one at a time, through the
// real TLS handshake: the gate is the listener plus the middleware, so a rule that
// only holds in a unit test is not a rule that holds in production.
func TestVerifierRules(t *testing.T) {
	const swarmID = "lm03ez1is3mlvt8zevoc2lfis"
	const nodeID = "ppbl900hxr1cyypgvzco2tpeu"

	ca := swarmtlstest.New(t)
	now := time.Now()
	serverPair := ca.Issue(t, swarmID, "n9xllyznqu52d6me0r6oeqhvv", "swarm-manager", now.Add(-time.Hour), now.Add(24*time.Hour))
	goodClient := ca.Issue(t, swarmID, nodeID, "swarm-worker", now.Add(-time.Hour), now.Add(24*time.Hour))
	wrongSwarm := ca.Issue(t, "some-other-swarm", nodeID, "swarm-worker", now.Add(-time.Hour), now.Add(24*time.Hour))
	removed := ca.Issue(t, swarmID, "deleted-node-id", "swarm-worker", now.Add(-time.Hour), now.Add(24*time.Hour))
	expired := ca.Issue(t, swarmID, nodeID, "swarm-worker", now.Add(-48*time.Hour), now.Add(-24*time.Hour))

	verifier := &Verifier{
		CA:      ca.Pool,
		Nodes:   fakeNodes{nodes: []runtime.Node{{ID: nodeID, Hostname: "nxt-sw-6-w", Role: "worker"}}},
		SwarmID: func(context.Context) (string, error) { return swarmID, nil },
		Log:     zerolog.Nop(),
	}
	handler := verifier.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, ok := FromContext(r.Context())
		if !ok {
			t.Error("handler ran without an identity")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, n.Hostname+"/"+n.ID)
	}))

	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = serverTLS(ca, serverPair.Pair)
	srv.StartTLS()
	defer srv.Close()

	// A transport error IS a rejection: the listener's RequireAndVerifyClientCert
	// refuses an expired or absent certificate at the handshake, before the
	// middleware can answer 403. Both outcomes are tested, neither is an accept.
	call := func(pair *tls.Certificate, query string) (int, string, error) {
		t.Helper()
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS(ca, pair)}, Timeout: 5 * time.Second}
		url := srv.URL + "/api/github/runners/assignment"
		if query != "" {
			url += "?" + query
		}
		resp, err := client.Get(url)
		if err != nil {
			return 0, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), nil
	}

	if status, body, err := call(&goodClient.Pair, ""); err != nil || status != http.StatusOK || body != "nxt-sw-6-w/"+nodeID {
		t.Fatalf("live node: status %d body %q err %v, want 200 and the certificate's node", status, body, err)
	}
	// A query parameter is not an identity: the answer is still the cert's node.
	if status, body, err := call(&goodClient.Pair, "node=some-other-host"); err != nil || status != http.StatusOK || body != "nxt-sw-6-w/"+nodeID {
		t.Fatalf("query parameter selected the caller: status %d body %q err %v", status, body, err)
	}
	for _, tc := range []struct {
		name string
		pair *tls.Certificate
	}{
		{"wrong swarm", &wrongSwarm.Pair},
		{"removed node", &removed.Pair},
	} {
		status, body, err := call(tc.pair, "")
		if err != nil {
			t.Fatalf("%s: rejected at the TLS layer (%v); want a 403 with a reason", tc.name, err)
		}
		if status != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403 (body %s)", tc.name, status, body)
		}
		if !strings.Contains(body, "reason") {
			t.Fatalf("%s: refusal should name a short reason: %s", tc.name, body)
		}
		if strings.Contains(body, nodeID) || strings.Contains(body, swarmID) {
			t.Fatalf("%s: the refusal leaks the accepted node/swarm ids: %s", tc.name, body)
		}
	}
	// An expired certificate fails at the handshake (RequireAndVerifyClientCert) and
	// would also be refused by the middleware — both are rejections, never an accept.
	if status, body, err := call(&expired.Pair, ""); err == nil && status == http.StatusOK {
		t.Fatalf("an expired client certificate was accepted: %s", body)
	}
	// No certificate at all: a handshake failure or a 403, never a 200.
	if status, body, err := call(nil, ""); err == nil && status == http.StatusOK {
		t.Fatalf("a caller without a certificate was accepted: %s", body)
	}
}

// TestVerifierExpiredCertificateIsRefused proves the middleware itself refuses an
// expired certificate, keeping "expired is rejected" a property of the rules rather
// than of a listener flag someone can flip.
func TestVerifierExpiredCertificateIsRefused(t *testing.T) {
	const swarmID = "swarm-id"
	ca := swarmtlstest.New(t)
	now := time.Now()
	pair := ca.Issue(t, swarmID, "node-id", "swarm-worker", now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	leaf, err := x509.ParseCertificate(pair.Pair.Certificate[0])
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	v := &Verifier{
		CA:      ca.Pool,
		Nodes:   fakeNodes{nodes: []runtime.Node{{ID: "node-id", Hostname: "w1"}}},
		SwarmID: func(context.Context) (string, error) { return swarmID, nil },
		Log:     zerolog.Nop(),
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	if _, reason := v.Authenticate(r); !strings.Contains(reason, "expired") {
		t.Fatalf("reason = %q, want an expired-certificate refusal", reason)
	}
}

// TestVerifierFailsClosed covers the cases where the verifier cannot decide: a
// half-configured verifier must refuse, never allow.
func TestVerifierFailsClosed(t *testing.T) {
	const swarmID = "swarm-id"
	const nodeID = "node-id"
	ca := swarmtlstest.New(t)
	now := time.Now()
	pair := ca.Issue(t, swarmID, nodeID, "swarm-worker", now.Add(-time.Hour), now.Add(time.Hour))
	leaf, err := x509.ParseCertificate(pair.Pair.Certificate[0])
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
		return r
	}
	live := func(context.Context) (string, error) { return swarmID, nil }

	cases := []struct {
		name string
		v    *Verifier
		tls  bool
		want string
	}{
		{"no CA pool", &Verifier{Nodes: fakeNodes{nodes: []runtime.Node{{ID: nodeID}}}, SwarmID: live}, true, "CA"},
		{"no node list", &Verifier{CA: ca.Pool, SwarmID: live}, true, "node list"},
		{"swarm id unavailable", &Verifier{CA: ca.Pool, Nodes: fakeNodes{nodes: []runtime.Node{{ID: nodeID}}},
			SwarmID: func(context.Context) (string, error) { return "", errors.New("docker busy") }}, true, "swarm id"},
		{"node list unavailable", &Verifier{CA: ca.Pool, Nodes: fakeNodes{err: errors.New("not a swarm manager")},
			SwarmID: live}, true, "node list"},
		{"no client certificate", &Verifier{CA: ca.Pool, Nodes: fakeNodes{nodes: []runtime.Node{{ID: nodeID}}}, SwarmID: live}, false, "no client certificate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.v.Log = zerolog.Nop()
			r := req()
			if !tc.tls {
				r.TLS = nil
			}
			if _, reason := tc.v.Authenticate(r); !strings.Contains(reason, tc.want) {
				t.Fatalf("reason = %q, want it to mention %q", reason, tc.want)
			}
		})
	}
}

// TestOnlyTheNodeRoutesAreServed asserts the node listener carries the routes it
// declares (the runner assignment, the node report, the cluster snapshot) and
// nothing else: a certificate-holding caller cannot reach any other path through
// it, because the verifier is mounted per route rather than around the mux.
//
// The paths are literals on purpose. Importing the packages that own them would
// create an import cycle (they all import this one), and the point of the test is
// the SHAPE — a mounted route answers, an unmounted one is a 404.
func TestOnlyTheNodeRoutesAreServed(t *testing.T) {
	const swarmID = "swarm-id"
	const nodeID = "node-id"
	ca := swarmtlstest.New(t)
	now := time.Now()
	serverPair := ca.Issue(t, swarmID, "manager-node", "swarm-manager", now.Add(-time.Hour), now.Add(time.Hour))
	clientPair := ca.Issue(t, swarmID, nodeID, "swarm-worker", now.Add(-time.Hour), now.Add(time.Hour))

	v := &Verifier{
		CA:      ca.Pool,
		Nodes:   fakeNodes{nodes: []runtime.Node{{ID: nodeID, Hostname: "w1"}}},
		SwarmID: func(context.Context) (string, error) { return swarmID, nil },
		Log:     zerolog.Nop(),
	}
	// The mux startAssignmentListener builds: the node-facing patterns, each one
	// wrapped by the verifier, and nothing else.
	mux := http.NewServeMux()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	for _, path := range []string{
		"/api/github/runners/assignment",
		"/internal/api/cluster/report",
		"/internal/api/cluster/snapshot",
	} {
		mux.Handle(path, v.Middleware(ok))
	}

	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = serverTLS(ca, serverPair.Pair)
	srv.StartTLS()
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS(ca, &clientPair.Pair)}, Timeout: 5 * time.Second}
	for _, path := range []string{"/", "/health", "/api/doctor", "/metrics", "/api/cluster/settings", "/api/github/runners", "/internal/api/cluster"} {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s answered %d through the node listener; only the declared routes may be served there", path, resp.StatusCode)
		}
	}
}

// TestLoadCA covers the CA candidates and the honest error when none exists.
func TestLoadCA(t *testing.T) {
	ca := swarmtlstest.New(t)
	dir := t.TempDir()
	path := ca.WriteCA(t, dir)
	pool, used, err := LoadCA(filepath.Join(dir, "missing.pem"), path)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if used != path || pool == nil {
		t.Fatalf("used = %q pool = %v", used, pool)
	}
	if _, _, err := LoadCA(filepath.Join(dir, "missing.pem")); err == nil {
		t.Fatal("a missing CA must be an error, not an empty pool")
	}
}

// TestCAPathsIncludeBothLayouts pins the two locations, because a fleet running an
// older Docker layout must not silently lose the endpoint.
func TestCAPathsIncludeBothLayouts(t *testing.T) {
	joined := strings.Join(CAPaths, " ")
	for _, want := range []string{
		"/var/lib/docker/swarm/ca.pem",
		"/var/lib/docker/swarm/certificates/swarm-root-ca.crt",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("CAPaths = %v, want %s", CAPaths, want)
		}
	}
}

// TestClientTLSVerifiesTheServer covers the worker's half: the server's certificate
// is checked against the swarm CA and the swarm O. Hostname verification is
// REPLACED (Docker's node certificates carry no IP SAN), which is why these two
// rules are the whole guarantee and are asserted explicitly.
func TestClientTLSVerifiesTheServer(t *testing.T) {
	const swarmID = "swarm-id"
	ca := swarmtlstest.New(t)
	otherCA := swarmtlstest.New(t)
	now := time.Now()
	client := ca.Issue(t, swarmID, "client-node", "swarm-worker", now.Add(-time.Hour), now.Add(time.Hour))
	certFile, keyFile := swarmtlstest.WriteNode(t, t.TempDir(), "client", client)

	cfg, err := ClientTLS(ClientConfig{CertFile: certFile, KeyFile: keyFile, CA: ca.Pool, SwarmID: swarmID})
	if err != nil {
		t.Fatalf("ClientTLS: %v", err)
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Fatal("ClientTLS must install VerifyPeerCertificate: verification is replaced, never disabled")
	}
	// Its own certificate verifies.
	if err := cfg.VerifyPeerCertificate(client.Pair.Certificate, nil); err != nil {
		t.Fatalf("the swarm's own certificate was refused: %v", err)
	}
	// A certificate from another swarm's CA is refused.
	foreign := otherCA.Issue(t, swarmID, "server", "swarm-manager", now.Add(-time.Hour), now.Add(time.Hour))
	if err := cfg.VerifyPeerCertificate(foreign.Pair.Certificate, nil); err == nil {
		t.Fatal("a server certificate from another CA was accepted")
	}
	// ... and so is one from this CA but for another swarm.
	otherSwarm := ca.Issue(t, "another-swarm", "server", "swarm-manager", now.Add(-time.Hour), now.Add(time.Hour))
	if err := cfg.VerifyPeerCertificate(otherSwarm.Pair.Certificate, nil); err == nil {
		t.Fatal("a certificate whose organisation is another swarm was accepted")
	}
	if _, err := ClientTLS(ClientConfig{CertFile: filepath.Join(t.TempDir(), "absent.crt"), KeyFile: keyFile, CA: ca.Pool}); err == nil {
		t.Fatal("a missing certificate must be an error, not a config without one")
	}
}
