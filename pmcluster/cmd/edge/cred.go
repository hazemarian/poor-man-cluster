package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/pullcred"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui"
)

// The credential listener: a second HTTP server on its own port (:9443),
// published with `mode: ingress`, serving exactly one route
// (pullcred.TokenPath) to exactly one kind of caller — a node of this swarm
// presenting its swarm certificate.
//
// It is deliberately NOT a gin/console route:
//
//   - the console's stack (session auth, CSRF, HTMX, the reverse proxy) is not
//     in front of it, so a node certificate cannot be used to reach a console
//     handler;
//   - the console's mux does not know the path, so the console's auth cannot be
//     bypassed by reaching this route through it (there is a test for that
//     in main_test.go).
//
// The listener's job is authentication and relaying. It cannot mint: the App's
// private key lives in the leader's daemon, so after verifying the node
// certificate it calls the daemon's Bearer-protected
// POST /api/github/pull-credential with the console's own token.

// Environment for the credential listener. The stack renders both; a
// standalone/local run leaves the listen address empty and the listener stays
// off, so `pmcluster-edge` on a laptop does not open a port. This swarm's ID is
// deliberately NOT an env var: the listener asks the daemon (see
// daemonAPI.SwarmID), which is the authority and which keeps a swarm re-init from
// changing the rendered compose.
const (
	envCredListen = "PMCLUSTER_CRED_LISTEN"    // e.g. ":9443"
	envCredCA     = "PMCLUSTER_CRED_CLIENT_CA" // the swarm root CA, bind-mounted read-only
)

// credClientTimeout bounds every call the listener makes to the daemon. A pull
// is waiting; a hung daemon must become a 502, not a stalled handshake.
const credClientTimeout = 5 * time.Second

// credConfig is the listener's whole configuration.
type credConfig struct {
	// Listen is the address to bind. Empty disables the listener.
	Listen string
	// ClientCA is the swarm root CA file, bind-mounted from the node.
	ClientCA string
	// DaemonURL and DaemonToken are the console's own API credentials — the
	// same ones the console uses for every other daemon call.
	DaemonURL   string
	DaemonToken string
	Log         zerolog.Logger
}

// credConfigFromEnv reads the listener settings. Defaults match the rendered
// stack so a hand-run in the container behaves like the deployed one.
func credConfigFromEnv(uiCfg ui.Config, log zerolog.Logger) credConfig {
	return credConfig{
		Listen:      os.Getenv(envCredListen),
		ClientCA:    envOrDefault(envCredCA, "/etc/pmcluster/swarm/swarm-root-ca.crt"),
		DaemonURL:   uiCfg.PMAPIURL,
		DaemonToken: uiCfg.PMAPIToken,
		Log:         log,
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// newCredentialServer builds the listener, or returns (nil, nil) when it is
// disabled. A configuration error is returned, never swallowed: an operator who
// set the listen address expects either a listener or a line saying why not.
func newCredentialServer(cfg credConfig) (*http.Server, error) {
	if cfg.Listen == "" {
		return nil, nil
	}
	// Fail closed on the two things that would make the route unable to
	// authenticate anyone: without a daemon token every mint would 401, and
	// without a daemon URL the node list and the swarm ID cannot be read — and a
	// listener that cannot read them must not accept any certificate.
	if cfg.DaemonToken == "" {
		return nil, errors.New("no daemon API token is configured (edge_api_token): the listener cannot mint a credential")
	}
	if cfg.DaemonURL == "" {
		return nil, errors.New("no daemon API URL is configured (UPSTREAM): the listener cannot mint a credential")
	}

	caPEM, err := pullcred.ReadCAFile(cfg.ClientCA)
	if err != nil {
		return nil, fmt.Errorf("%w (the stack bind-mounts it from /var/lib/docker/swarm/certificates)", err)
	}
	// No server identity to prove: the client authenticates, not the server.
	// See pullcred.Remote.Get for the reasoning.
	serverCert, err := pullcred.SelfSignedServerCert("127.0.0.1", "localhost")
	if err != nil {
		return nil, err
	}
	deps := &daemonAPI{url: cfg.DaemonURL, token: cfg.DaemonToken, client: &http.Client{Timeout: credClientTimeout}}
	verifier := &pullcred.Verifier{SwarmIDFunc: deps.SwarmID, NodeIDs: deps.NodeIDs}
	tlsCfg, err := verifier.TLSConfig(caPEM, serverCert)
	if err != nil {
		return nil, err
	}

	srv := &pullcred.Server{Verifier: verifier, Tokens: deps, Log: &cfg.Log}
	return &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}, nil
}

// daemonAPI is the listener's client for the leader's daemon: it mints the
// credential and supplies the node list the certificate rule needs.
type daemonAPI struct {
	url    string
	token  string
	client *http.Client

	mu      sync.Mutex
	ids     []string
	fetched time.Time
	swarmID string
}

// nodeListTTL is how long a node list is reused. A node that left the swarm a
// few seconds ago may still authenticate for this long; that is a deliberate
// trade for not asking the daemon on every pull. It is a cache, not a grant:
// the certificate itself must still chain to the swarm CA.
const nodeListTTL = 30 * time.Second

// SwarmID returns this swarm's ID, read once from the daemon and kept for the
// life of the process: a cluster's Raft ID never changes, and a re-initialised
// swarm means every node rejoins (and the edge restarts with them).
//
// A failed read is an error, not an empty string: the verifier turns it into a
// refusal, which is the correct answer when the listener cannot say which swarm
// it belongs to.
func (d *daemonAPI) SwarmID(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.swarmID != "" {
		return d.swarmID, nil
	}
	var body struct {
		Swarm struct {
			ID string `json:"id"`
		} `json:"swarm"`
	}
	if err := d.do(ctx, http.MethodGet, "/api/cluster/info", nil, &body); err != nil {
		return "", err
	}
	if body.Swarm.ID == "" {
		return "", errors.New("the daemon reports no swarm ID — is this node still part of a swarm?")
	}
	d.swarmID = body.Swarm.ID
	return d.swarmID, nil
}

// NodeIDs returns the current swarm node IDs, cached for nodeListTTL. A failed
// lookup returns the error — the verifier refuses rather than guessing.
func (d *daemonAPI) NodeIDs(ctx context.Context) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ids != nil && time.Since(d.fetched) < nodeListTTL {
		return d.ids, nil
	}
	var body struct {
		Nodes []struct {
			ID string `json:"id"`
		} `json:"nodes"`
	}
	if err := d.do(ctx, http.MethodGet, "/api/nodes", nil, &body); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(body.Nodes))
	for _, n := range body.Nodes {
		if n.ID != "" {
			ids = append(ids, n.ID)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("the daemon returned an empty node list")
	}
	d.ids, d.fetched = ids, time.Now()
	return ids, nil
}

// PullToken mints one GHCR credential through the daemon. The daemon is the
// only process holding the App key; this is the call that uses it.
func (d *daemonAPI) PullToken(ctx context.Context, server string) (pullcred.Token, error) {
	var out struct {
		Username  string `json:"username"`
		Secret    string `json:"secret"`
		ExpiresAt string `json:"expires_at"`
	}
	err := d.do(ctx, http.MethodPost, "/api/github/pull-credential", map[string]string{"server": server}, &out)
	if err != nil {
		return pullcred.Token{}, err
	}
	if out.Secret == "" {
		return pullcred.Token{}, errors.New("the daemon returned no token")
	}
	tok := pullcred.Token{Username: out.Username, Secret: out.Secret}
	if out.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, out.ExpiresAt); err == nil {
			tok.ExpiresAt = t
		}
	}
	return tok, nil
}

// do performs one daemon call. Errors name the route and carry the daemon's own
// message, which is what reaches the pull's stderr.
func (d *daemonAPI) do(ctx context.Context, method, path string, in any, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode %s request: %w", path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.url+path, body)
	if err != nil {
		return fmt.Errorf("build %s request: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("call the daemon %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("read %s response: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(payload, &e) == nil && e.Error != "" {
			return fmt.Errorf("the daemon refused %s (%s): %s", path, resp.Status, e.Error)
		}
		return fmt.Errorf("the daemon answered %s %s", path, resp.Status)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}
