package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
)

// TestCertificateBuysNothingOnTheMainAPI is the scope guarantee of the swarm-TLS
// channel, asserted from the other side: a request that carries a (here simulated)
// verified swarm client certificate still has to authenticate like anyone else on
// /api. The certificate is accepted ONLY on the runner-assignment listener, which is
// a different server with a different mux — so a compromised worker cannot use it to
// read a second endpoint.
func TestCertificateBuysNothingOnTheMainAPI(t *testing.T) {
	lookup := &fakeLookup{users: map[string]*auth.User{"valid": {ID: 1, Name: "alice"}}}
	srv := httptest.NewServer(New(Deps{Lookup: lookup}))
	t.Cleanup(srv.Close)

	// The assignment route lives on the mTLS listener (internal/cli), never here.
	for _, path := range []string{
		"/api/github/runners/assignment",
		"/api/github/runners",
		"/api/cluster/settings",
		"/api/doctor",
		"/api/me",
	} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		// A client certificate would arrive in req.TLS.PeerCertificates; the router
		// does not look at it, and must answer 401 without a Bearer token.
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		_ = resp.Body.Close()
		// Every one of them is behind Bearer: the certificate that opens the
		// assignment listener opens nothing here.
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s answered %d, want 401: the swarm certificate must buy nothing on the main API", path, resp.StatusCode)
		}
	}
}

// TestTheInternalNodeRoutesAreNotMountedHere is the scope guarantee for the routes
// that live on the mTLS listener: the snapshot is the one answer that carries
// cluster settings (among them a few secret values), so it must not exist on the
// operator-facing API at all — not behind a token, not behind anything.
//
// 404 is the assertion, not 401: a route that is merely *forbidden* here is still a
// route on the wrong surface, one auth bug away from being readable.
func TestTheInternalNodeRoutesAreNotMountedHere(t *testing.T) {
	lookup := &fakeLookup{users: map[string]*auth.User{"valid": {ID: 1, Name: "alice"}}}
	srv := httptest.NewServer(New(Deps{Lookup: lookup}))
	t.Cleanup(srv.Close)

	for _, path := range []string{
		"/internal/api/cluster/snapshot",
		"/internal/api/cluster/report",
		"/internal/api/join/key",
	} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		// Even WITH an admin bearer, the answer must be "no such route": these use
		// the node certificate, and a token must not be a substitute for it.
		req.Header.Set("Authorization", "Bearer valid")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body := make([]byte, 512)
		n, _ := resp.Body.Read(body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s answered %d on the public API, want 404: node-facing routes live on the mTLS listener only", path, resp.StatusCode)
		}
		if strings.Contains(string(body[:n]), "tables") {
			t.Fatalf("%s leaked a snapshot-shaped body on the public API: %s", path, body[:n])
		}
	}
}
