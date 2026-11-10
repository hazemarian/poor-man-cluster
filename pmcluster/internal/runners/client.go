package runners

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth/swarmtls"
)

// ErrNoSwarmCertificate means this host is not a swarm node (or Docker has not
// provisioned its certificate yet). The caller logs ONE warning per boot and
// skips the poll: there is no unauthenticated fallback, by design — an anonymous
// "what does this cluster run" endpoint is exactly what this design refuses to
// add.
var ErrNoSwarmCertificate = errors.New("no swarm node certificate on this host: the runner assignment is only readable by a node of the cluster")

// Client fetches this node's runner assignment from the leader.
//
// Transport, in order (the first that answers wins, and the choice is logged once
// per boot):
//
//  1. a MANAGER's swarm data-path address, taken from the live swarm
//     (`docker info`, RemoteManagers) — these are the addresses Swarm itself uses
//     between nodes, they need no published port, no firewall change and no DNS,
//     and they are the address this listener is bound to;
//  2. `127.0.0.1:<port>` — the ingress-published address, if anything ever
//     publishes this port on the node. It is kept because it is the reachability
//     story an operator asked for explicitly, and because a node-local forwarder
//     is a legitimate deployment; it is second because it does not exist in the
//     default stack (the assignment listener belongs to the host daemon, and a
//     swarm ingress port can only target a container task).
//
// A SERVICE NAME (`pmcluster-edge`) is deliberately not a candidate: measured on
// the live fleet a host process cannot resolve or route an overlay service name
// (the embedded DNS is per-container), and the console in front of it is a plain
// HTTP proxy, so it cannot carry this mTLS channel either.
type Client struct {
	// CertFile/KeyFile are this node's swarm certificate (Docker-provisioned).
	CertFile string
	KeyFile  string
	// CAPaths are the swarm CA candidates; empty means swarmtls.CAPaths.
	CAPaths []string
	// Port is the assignment port (default runners.AssignmentPort).
	Port int
	// Managers returns candidate hosts, most useful first (a node's own leader
	// first is impossible to know from a worker, so the caller returns every
	// manager address; the non-leader managers do not listen and are skipped).
	Managers func(ctx context.Context) ([]string, error)
	// Loopback is the published-address fallback (default 127.0.0.1).
	Loopback string
	// HTTP overrides the client (tests).
	HTTP *http.Client
	Log  zerolog.Logger
}

func (c *Client) port() int {
	if c.Port > 0 {
		return c.Port
	}
	return AssignmentPort
}

// candidates returns the ordered base URLs to try.
func (c *Client) candidates(ctx context.Context) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(host string) {
		host = strings.TrimSpace(host)
		if host == "" {
			return
		}
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		base := fmt.Sprintf("https://%s:%d", host, c.port())
		if seen[base] {
			return
		}
		seen[base] = true
		out = append(out, base)
	}
	if c.Managers != nil {
		hosts, err := c.Managers(ctx)
		if err != nil {
			return nil, fmt.Errorf("read the swarm manager addresses: %w", err)
		}
		for _, h := range hosts {
			add(h)
		}
	}
	loop := strings.TrimSpace(c.Loopback)
	if loop == "" {
		loop = "127.0.0.1"
	}
	add(loop)
	return out, nil
}

// Fetch resolves the swarm credential, tries every candidate, and returns the
// assignment plus the base URL that answered.
func (c *Client) Fetch(ctx context.Context) (Assignment, string, error) {
	certFile, keyFile := c.CertFile, c.KeyFile
	if certFile == "" {
		certFile = swarmtls.FirstFile(swarmtls.NodeCertPaths)
	}
	if keyFile == "" {
		keyFile = swarmtls.FirstFile(swarmtls.NodeKeyPaths)
	}
	if certFile == "" || keyFile == "" {
		return Assignment{}, "", ErrNoSwarmCertificate
	}
	pool, _, err := swarmtls.LoadCA(c.CAPaths...)
	if err != nil {
		return Assignment{}, "", fmt.Errorf("%w (%v)", ErrNoSwarmCertificate, err)
	}
	swarmID, err := certSwarmID(certFile)
	if err != nil {
		return Assignment{}, "", fmt.Errorf("read this node's swarm certificate: %w", err)
	}
	tlsCfg, err := swarmtls.ClientTLS(swarmtls.ClientConfig{
		CertFile: certFile,
		KeyFile:  keyFile,
		CA:       pool,
		// The node's own certificate names the swarm it belongs to; a worker
		// cannot read the cluster block of `docker info`, and does not need to.
		SwarmID: swarmID,
	})
	if err != nil {
		return Assignment{}, "", fmt.Errorf("%w: %v", ErrNoSwarmCertificate, err)
	}

	bases, err := c.candidates(ctx)
	if err != nil {
		return Assignment{}, "", err
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig:   tlsCfg,
				DisableKeepAlives: true, // one pull, one connection, closed
			},
			// Bounded: a leader that is mid-failover must not stall the loop.
			Timeout: 10 * time.Second,
		}
	}

	var errs []string
	for _, base := range bases {
		a, err := c.get(ctx, client, base)
		if err == nil {
			return a, base, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", base, err))
	}
	if len(errs) == 0 {
		return Assignment{}, "", errors.New("no candidate address for the leader")
	}
	return Assignment{}, "", fmt.Errorf("could not read this node's runner assignment (%s)", strings.Join(errs, "; "))
}

// get performs one request against one candidate.
func (c *Client) get(ctx context.Context, client *http.Client, base string) (Assignment, error) {
	url := base + AssignmentPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Assignment{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Assignment{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return Assignment{}, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusConflict:
		// The leader answered with a refusal: the stored settings and the node
		// set disagree. That is an answer, not a transport failure — surface it
		// so the node's log names it instead of retrying another manager.
		return Assignment{}, fmt.Errorf("the leader refused this node's assignment: %s", apiErrorText(body))
	case http.StatusForbidden:
		return Assignment{}, fmt.Errorf("the leader refused this node's certificate: %s", apiErrorText(body))
	default:
		return Assignment{}, fmt.Errorf("%s: %s", resp.Status, apiErrorText(body))
	}
	var a Assignment
	if err := json.Unmarshal(body, &a); err != nil {
		return Assignment{}, fmt.Errorf("decode the assignment: %w", err)
	}
	return a, nil
}

func apiErrorText(body []byte) string {
	var e struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &e); err == nil {
		msg := strings.TrimSpace(e.Error)
		if r := strings.TrimSpace(e.Reason); r != "" {
			if msg != "" {
				msg += " (" + r + ")"
			} else {
				msg = r
			}
		}
		if msg != "" {
			return msg
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// certSwarmID reads the swarm ID (the certificate's organisation) from this
// node's own certificate.
func certSwarmID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			return "", fmt.Errorf("no certificate in %s", path)
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", err
		}
		if id := swarmtls.SwarmIDOf(cert); id != "" {
			return id, nil
		}
		return "", fmt.Errorf("certificate in %s carries no swarm organisation", path)
	}
}
