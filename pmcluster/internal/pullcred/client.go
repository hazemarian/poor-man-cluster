package pullcred

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Remote is the helper's client: it dials the credential listener, presents
// this node's Swarm certificate, and returns the minted token.
//
// It is deliberately tiny and synchronous. A pull is already waiting on it, the
// listener is one mesh hop away, and DefaultTimeout (2s) bounds the whole call
// so a dead edge becomes a fast failure Docker can report rather than a pull
// that hangs.
type Remote struct {
	// URL is the listener address. Empty uses DefaultURL.
	URL string
	// CertDir is where this node's swarm certificate and key live. Empty uses
	// SwarmCertDir. The helper runs as root on the host (dockerd invokes
	// credential helpers as the user running docker), so /var/lib/docker/swarm
	// is readable.
	CertDir string
	// Timeout bounds the whole exchange. Zero uses DefaultTimeout.
	Timeout time.Duration
	// HTTPClient overrides the transport (tests). When set, URL/CertDir/Timeout
	// only supply defaults for empty fields.
	HTTPClient *http.Client
}

// Get asks the cluster for a credential for server.
func (r Remote) Get(ctx context.Context, server string) (Token, error) {
	if server == "" {
		return Token{}, fmt.Errorf("no registry URL")
	}
	url := r.URL
	if url == "" {
		url = DefaultURL
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	client := r.HTTPClient
	if client == nil {
		// The server side of this TLS connection has no durable identity to
		// check: the edge container holds no swarm certificate and there is
		// deliberately no second CA to distribute. What authenticates the
		// exchange is the CLIENT certificate — this node's swarm cert, verified
		// by the listener against the swarm root CA — and the token it yields is
		// short-lived. Verification is therefore skipped on this leg, and the
		// connection is to 127.0.0.1 over the cluster's own mesh.
		cfg := &tls.Config{
			MinVersion: tls.VersionTLS12,
			//nolint:gosec // see the comment above: the listener's identity is
			// the client certificate it demands, not a server name we can pin.
			InsecureSkipVerify: true,
		}
		certDir := r.CertDir
		if certDir == "" {
			certDir = SwarmCertDir
		}
		cert, err := tls.LoadX509KeyPair(
			filepath.Join(certDir, "swarm-node.crt"),
			filepath.Join(certDir, "swarm-node.key"),
		)
		if err != nil {
			return Token{}, fmt.Errorf("load this node's swarm certificate from %s: %w", certDir, err)
		}
		cfg.Certificates = []tls.Certificate{cert}
		client = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig:     cfg,
				ForceAttemptHTTP2:   false,
				DisableKeepAlives:   true,
				TLSHandshakeTimeout: timeout,
				DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
			},
		}
	}

	body, err := json.Marshal(Request{Server: server})
	if err != nil {
		return Token{}, fmt.Errorf("encode request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(url, "/")+TokenPath, bytes.NewReader(body))
	if err != nil {
		return Token{}, fmt.Errorf("build request to %s: %w", url, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("reach the credential listener at %s (is pmcluster-edge running on the leader?): %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes))
	if err != nil {
		return Token{}, fmt.Errorf("read credential response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Token{}, fmt.Errorf("credential listener answered %s: %s", resp.Status, errorText(payload))
	}
	var tok Token
	if err := json.Unmarshal(payload, &tok); err != nil {
		return Token{}, fmt.Errorf("credential listener returned invalid JSON: %w", err)
	}
	if tok.Secret == "" {
		return Token{}, fmt.Errorf("credential listener returned no token")
	}
	if tok.Username == "" {
		tok.Username = Username
	}
	return tok, nil
}

// errorText extracts the listener's {"error": ...} message, falling back to the
// raw (bounded) body.
func errorText(payload []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(payload, &e); err == nil && e.Error != "" {
		return e.Error
	}
	s := strings.TrimSpace(string(payload))
	if s == "" {
		return "(empty response)"
	}
	return s
}

// ReadCAFile reads the swarm root CA from disk for the listener's TLS config.
func ReadCAFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read swarm root CA %s: %w", path, err)
	}
	return b, nil
}
