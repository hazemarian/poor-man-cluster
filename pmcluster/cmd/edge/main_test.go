package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/edgeproxy"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/pullcred"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui"
)

// consoleApp builds a console with an in-memory store and a daemon URL that is
// never dialled: these tests inspect routing, not upstream behaviour.
func consoleApp(t *testing.T) *ui.App {
	t.Helper()
	cfg := ui.FromEnv()
	cfg.DataDir = t.TempDir()
	cfg.PMAPIURL = "http://127.0.0.1:1"
	cfg.PMAPIToken = "pmc_test"
	cfg.SessionSecret = []byte("0123456789abcdefgh")
	cfg.CookieName = "pmui_session"
	cfg.LoginDisabled = true
	app, err := ui.NewApp(cfg)
	if err != nil {
		t.Fatalf("ui.NewApp: %v", err)
	}
	return app
}

// TestCredentialRouteIsNotOnTheConsoleMux is the isolation the design depends
// on, asserted against the REAL console handler the edge serves: the credential
// path is not one of its routes, so no request can reach it through the
// console's auth, and no console handler can be reached through the credential
// listener.
func TestCredentialRouteIsNotOnTheConsoleMux(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	app := consoleApp(t)
	handler := combineProxyAndUI(edgeproxy.Config{
		Upstream:       "http://127.0.0.1:1",
		RequestTimeout: time.Second,
		MaxConcurrent:  4,
		MaxBodyBytes:   1 << 20,
		TrustedCIDRs:   []string{"127.0.0.1/8"},
	}, app)

	engine, ok := handler.(*gin.Engine)
	if !ok {
		t.Fatalf("console handler is %T, want *gin.Engine", handler)
	}
	for _, route := range engine.Routes() {
		if route.Path == pullcred.TokenPath {
			t.Fatalf("the console mux serves %s (%s): a node certificate must not reach a console handler",
				pullcred.TokenPath, route.Method)
		}
	}

	// And the credential listener is not a second door into the console: it
	// answers the one path and 404s everything else.
	cred := (&pullcred.Server{
		Verifier: &pullcred.Verifier{SwarmID: "swarm", NodeIDs: func(context.Context) ([]string, error) {
			return []string{"node"}, nil
		}},
		Tokens: fakeTokenSource{},
	})
	for _, path := range []string{"/", "/web/", "/api/ghcr-token", "/healthz"} {
		rr := httptest.NewRecorder()
		cred.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusNotFound {
			t.Errorf("credential listener served %s with %d, want 404", path, rr.Code)
		}
	}
	rr := httptest.NewRecorder()
	cred.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, pullcred.TokenPath, strings.NewReader(`{"server":"ghcr.io"}`)))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("%s without a client certificate = %d, want 401", pullcred.TokenPath, rr.Code)
	}
}

type fakeTokenSource struct{}

func (fakeTokenSource) PullToken(context.Context, string) (pullcred.Token, error) {
	return pullcred.Token{Username: pullcred.Username, Secret: "ghs_test", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// TestCredentialListenerIsDisabledWithoutItsInputs: an incomplete listener must
// refuse to start, naming the missing setting, rather than serve a route that
// cannot authenticate anyone.
func TestCredentialListenerIsDisabledWithoutItsInputs(t *testing.T) {
	log := zerolog.Nop()
	base := credConfig{
		Listen:      ":9443",
		ClientCA:    "/nonexistent/swarm-root-ca.crt",
		DaemonURL:   "http://127.0.0.1:9090",
		DaemonToken: "tok",
		Log:         log,
	}

	if srv, err := newCredentialServer(credConfig{Listen: ""}); srv != nil || err != nil {
		t.Errorf("disabled listener = (%v, %v), want (nil, nil)", srv, err)
	}

	noToken := base
	noToken.DaemonToken = ""
	if _, err := newCredentialServer(noToken); err == nil {
		t.Error("a listener without a daemon token started")
	}

	noURL := base
	noURL.DaemonURL = ""
	if _, err := newCredentialServer(noURL); err == nil {
		t.Error("a listener that cannot read this swarm's ID or node list started")
	}

	// The CA is missing (nothing at that path): the error names the file.
	if _, err := newCredentialServer(base); err == nil || !strings.Contains(err.Error(), base.ClientCA) {
		t.Errorf("missing CA: %v, want an error naming %s", err, base.ClientCA)
	}
}

// TestDaemonAPISwarmIDIsReadOnceAndFailsLoudly: the certificate rule needs the
// swarm ID, and a failed read must surface as an error the verifier can refuse
// on — never as an empty ID that matches every certificate.
func TestDaemonAPISwarmIDIsReadOnceAndFailsLoudly(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/cluster/info" {
			t.Errorf("path = %q, want /api/cluster/info", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"node_name":"nxt-sw-1-m","swarm":{"state":"active","id":"lm03ez1is3mlvt8zevoc2lfis"}}`))
	}))
	defer srv.Close()

	d := &daemonAPI{url: srv.URL, token: "tok", client: srv.Client(), swarmID: ""}
	id, err := d.SwarmID(context.Background())
	if err != nil {
		t.Fatalf("SwarmID: %v", err)
	}
	if id != "lm03ez1is3mlvt8zevoc2lfis" {
		t.Errorf("id = %q", id)
	}
	if _, err := d.SwarmID(context.Background()); err != nil {
		t.Fatalf("second SwarmID: %v", err)
	}
	if calls != 1 {
		t.Errorf("swarm ID fetched %d times, want 1 (it never changes)", calls)
	}

	// A daemon that cannot say is an error, not an empty ID.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"swarm":{"state":"inactive"}}`))
	}))
	defer empty.Close()
	if _, err := (&daemonAPI{url: empty.URL, token: "tok", client: empty.Client()}).SwarmID(context.Background()); err == nil {
		t.Error("SwarmID returned no error from a daemon in no swarm")
	}
}

// TestDaemonAPICachesTheNodeList: the certificate rule reads the node list on
// every handshake, so it is cached — but a failed read is not cached and is
// returned, because the verifier must refuse rather than verify nothing.
func TestDaemonAPICachesTheNodeList(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want the console token", got)
		}
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"nodes":[{"id":"node-a"},{"id":"node-b"}]}`))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"docker unreachable"}`))
	}))
	defer srv.Close()

	d := &daemonAPI{url: srv.URL, token: "tok", client: srv.Client()}
	ids, err := d.NodeIDs(context.Background())
	if err != nil {
		t.Fatalf("NodeIDs: %v", err)
	}
	if len(ids) != 2 || ids[0] != "node-a" {
		t.Fatalf("ids = %v, want node-a, node-b", ids)
	}
	if _, err := d.NodeIDs(context.Background()); err != nil {
		t.Fatalf("second NodeIDs: %v", err)
	}
	if calls != 1 {
		t.Errorf("node list fetched %d times, want 1 (it must be cached)", calls)
	}
}

// TestDaemonAPIMintsThroughThePullCredentialRoute pins the request shape and
// the RFC3339 expiry the listener relays to the helper.
func TestDaemonAPIMintsThroughThePullCredentialRoute(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want the console token", got)
		}
		var body struct {
			Server string `json:"server"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body.Server != pullcred.Registry {
			t.Errorf("server = %q, want %q", body.Server, pullcred.Registry)
		}
		expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"username":   pullcred.Username,
			"secret":     "ghs_edge_test",
			"expires_at": expiry.Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	d := &daemonAPI{url: srv.URL, token: "tok", client: srv.Client()}
	tok, err := d.PullToken(context.Background(), pullcred.Registry)
	if err != nil {
		t.Fatalf("PullToken: %v", err)
	}
	if gotPath != "/api/github/pull-credential" {
		t.Errorf("path = %q, want /api/github/pull-credential", gotPath)
	}
	if tok.Secret != "ghs_edge_test" || tok.Username != pullcred.Username {
		t.Fatalf("token = %+v", tok)
	}
	if tok.ExpiresAt.Before(time.Now()) {
		t.Errorf("expires_at = %s, want the parsed future expiry", tok.ExpiresAt)
	}
}

// TestDaemonAPISurfacesARefusal: the daemon's own sentence reaches the pull.
func TestDaemonAPISurfacesARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"no GitHub App configured (set one with ` + "`pmcluster github app set`" + `)"}`))
	}))
	defer srv.Close()

	d := &daemonAPI{url: srv.URL, token: "tok", client: srv.Client()}
	_, err := d.PullToken(context.Background(), pullcred.Registry)
	if err == nil {
		t.Fatal("PullToken succeeded against a refusing daemon")
	}
	if !strings.Contains(err.Error(), "github app set") {
		t.Errorf("error = %v, want the daemon's message to reach the caller", err)
	}
}
