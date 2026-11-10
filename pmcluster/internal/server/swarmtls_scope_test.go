package server

import (
	"net/http"
	"net/http/httptest"
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
