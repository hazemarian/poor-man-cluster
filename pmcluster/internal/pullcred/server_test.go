package pullcred

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// fakeTokens is a TokenSource with a fixed answer.
type fakeTokens struct {
	tok    Token
	err    error
	called int
	server string
}

func (f *fakeTokens) PullToken(_ context.Context, server string) (Token, error) {
	f.called++
	f.server = server
	return f.tok, f.err
}

// credRequest builds a request the way the listener sees one after the TLS
// handshake, including the peer certificate when leaf is non-nil.
func credRequest(body string, leaf *x509.Certificate) *http.Request {
	r := httptest.NewRequest(http.MethodPost, TokenPath, strings.NewReader(body))
	if leaf != nil {
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	}
	return r
}

func TestServerServesATokenToANodeOfThisSwarm(t *testing.T) {
	ca := newTestAuthority(t, "swarm-ca")
	_, leaf := ca.newNodeCert(t, testSwarmID, testNodeID)
	expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	tokens := &fakeTokens{tok: Token{Username: Username, Secret: "ghs_pull_test", ExpiresAt: expiry}}

	s := &Server{Verifier: newVerifier(testNodeID), Tokens: tokens}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, credRequest(`{"server":"ghcr.io"}`, leaf))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	var got Token
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rr.Body.String())
	}
	if got.Secret != "ghs_pull_test" || got.Username != Username {
		t.Fatalf("token = %+v, want the minted one", got)
	}
	if !got.ExpiresAt.Equal(expiry) {
		t.Errorf("expires_at = %s, want %s", got.ExpiresAt, expiry)
	}
	if tokens.server != Registry {
		t.Errorf("token source asked for %q, want %q", tokens.server, Registry)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store — a token must not be cached", cc)
	}
	if rr.Header().Get("Set-Cookie") != "" {
		t.Error("the credential route set a cookie")
	}
}

func TestServerRejectsAMissingCertificate(t *testing.T) {
	tokens := &fakeTokens{tok: Token{Secret: "ghs_never"}}
	s := &Server{Verifier: newVerifier(testNodeID), Tokens: tokens}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, credRequest(`{"server":"ghcr.io"}`, nil))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if tokens.called != 0 {
		t.Error("a token was minted for a connection with no client certificate")
	}
	if strings.Contains(rr.Body.String(), "ghs_") {
		t.Error("the refusal body carries a token")
	}
}

func TestServerRejectsAnUnlistedNode(t *testing.T) {
	ca := newTestAuthority(t, "swarm-ca")
	_, leaf := ca.newNodeCert(t, testSwarmID, "a-node-that-left")
	tokens := &fakeTokens{tok: Token{Secret: "ghs_never"}}
	s := &Server{Verifier: newVerifier(testNodeID), Tokens: tokens}

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, credRequest(`{"server":"ghcr.io"}`, leaf))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if tokens.called != 0 {
		t.Error("a token was minted for a node outside the node list")
	}
}

// TestServerRefusesARegistryItDoesNotHold: the installation token is a GHCR
// credential; asking for another host must not hand it over.
func TestServerRefusesARegistryItDoesNotHold(t *testing.T) {
	ca := newTestAuthority(t, "swarm-ca")
	_, leaf := ca.newNodeCert(t, testSwarmID, testNodeID)
	tokens := &fakeTokens{tok: Token{Secret: "ghs_never"}}
	s := &Server{Verifier: newVerifier(testNodeID), Tokens: tokens}

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, credRequest(`{"server":"docker.io"}`, leaf))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if tokens.called != 0 {
		t.Error("a GHCR token was minted for another registry")
	}
	if !strings.Contains(rr.Body.String(), Registry) {
		t.Errorf("refusal %q does not name the registry that is served", rr.Body.String())
	}
}

func TestServerRoutesAreOnlyTheCredentialPath(t *testing.T) {
	s := &Server{Verifier: newVerifier(testNodeID), Tokens: &fakeTokens{}}
	h := s.Handler()

	// Anything else is a 404 from this mux: the listener is not a second door
	// into the console or the daemon.
	for _, path := range []string{"/", "/web/", "/healthz", "/cred/v1/token/extra"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, rr.Code)
		}
	}

	// The credential path takes POST only.
	ca := newTestAuthority(t, "swarm-ca")
	_, leaf := ca.newNodeCert(t, testSwarmID, testNodeID)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, credRequest(`{}`, leaf).WithContext(context.Background()))
	if rr.Code == http.StatusMethodNotAllowed {
		return
	}
	get := credRequest("", leaf)
	get.Method = http.MethodGet
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, get)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET %s = %d, want 405", TokenPath, rr.Code)
	}
	if allow := rr.Header().Get("Allow"); allow != http.MethodPost {
		t.Errorf("Allow = %q, want POST", allow)
	}
}

// TestServerNeverPutsTheTokenInALogLine: the log carries the node, the outcome
// and the expiry — never the credential.
func TestServerNeverPutsTheTokenInALogLine(t *testing.T) {
	ca := newTestAuthority(t, "swarm-ca")
	_, leaf := ca.newNodeCert(t, testSwarmID, testNodeID)
	var buf bytes.Buffer
	log := zerolog.New(&buf)
	s := &Server{
		Verifier: newVerifier(testNodeID),
		Tokens:   &fakeTokens{tok: Token{Secret: "ghs_do_not_log", ExpiresAt: time.Now().Add(time.Hour)}},
		Log:      &log,
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, credRequest(`{"server":"ghcr.io"}`, leaf))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if strings.Contains(buf.String(), "ghs_do_not_log") {
		t.Fatalf("the token reached the log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), testNodeID) {
		t.Errorf("the log line does not name the node it served: %s", buf.String())
	}
}

// TestServerReportsAMintFailureToTheCaller keeps the reason actionable: a
// pull failing with "unauthorized" is a day of debugging, "the App key was
// rejected" is not.
func TestServerReportsAMintFailureToTheCaller(t *testing.T) {
	ca := newTestAuthority(t, "swarm-ca")
	_, leaf := ca.newNodeCert(t, testSwarmID, testNodeID)
	s := &Server{
		Verifier: newVerifier(testNodeID),
		Tokens:   &fakeTokens{err: errors.New("the daemon's GitHub App key was rejected")},
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, credRequest(`{"server":"ghcr.io"}`, leaf))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "key was rejected") {
		t.Errorf("body = %q, want the mint failure to reach the caller", rr.Body.String())
	}
}

// TestServerDefaultsTheUsernameAndExpiry keeps the response shape stable even
// when a token source omits them.
func TestServerDefaultsTheUsernameAndExpiry(t *testing.T) {
	ca := newTestAuthority(t, "swarm-ca")
	_, leaf := ca.newNodeCert(t, testSwarmID, testNodeID)
	s := &Server{Verifier: newVerifier(testNodeID), Tokens: &fakeTokens{tok: Token{Secret: "ghs_x"}}}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, credRequest(`{"server":"ghcr.io"}`, leaf))
	var got Token
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Username != Username {
		t.Errorf("username = %q, want %q", got.Username, Username)
	}
	if got.ExpiresAt.IsZero() || got.ExpiresAt.Before(time.Now()) {
		t.Errorf("expires_at = %s, want a future time", got.ExpiresAt)
	}
}
