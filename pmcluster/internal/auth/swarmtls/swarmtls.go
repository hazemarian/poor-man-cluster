// Package swarmtls authenticates a call by the certificate Docker Swarm already
// gave the caller, and by nothing else.
//
// Why a client certificate and not a shared token: a worker cannot read cluster
// settings (Docker configs are manager-only), so there is no channel through
// which the leader could hand a worker a shared secret, and a token that has to
// be copied to a node by hand is a token that goes stale on the first node
// replacement. Every swarm node already holds a CA-signed, per-node certificate
// with a one-day-lived rotation (Docker renews it), and the swarm ID as its
// organisation — so the identity, the cluster scope and the revocation path all
// exist before this package runs:
//
//   - CA-signed chain            → the caller is a node of a swarm running this CA.
//   - O == this cluster's ID     → ... this swarm, not another one (a node from a
//     different swarm has a valid chain, but a different O).
//   - CN is a live node ID       → `docker node rm` removes it from the node list,
//     and that IS the revocation: a removed node's certificate stops being
//     accepted the moment the node list no longer contains it.
//   - not expired / not yet valid → an expired certificate is a rejected
//     certificate, never a warning.
//
// What this package deliberately is NOT, so it does not grow into one:
//
//   - It is not a second control plane. It authenticates ONE read-only pull
//     (the runner assignment) and is mounted on that route only. If a later
//     change wants a second endpoint over this channel, that is a redesign, not
//     an extension — see the comment at the mount site.
//   - It does not carry settings, credentials or secrets. The handler's response
//     schema is the assignment spec and nothing else.
//   - It does not authenticate users, mutate anything, push to workers, or hold
//     a long-lived connection. One request, one answer, close.
//
// The honest trade: an mTLS listener on the swarm data-path address is a port
// this process now opens where it previously opened none. It is bound to the
// node's overlay address (never a public host port), it requires a certificate
// no machine outside the swarm can obtain, and the host firewall rule that
// covers it is in harden-host.sh with the rest of the internal ports.
package swarmtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// CAPaths are the swarm root CA locations, in the order Docker has used them.
// ca.pem is the historical location; Docker 28+ keeps the same certificate in
// the node's certificate directory, which every node (worker or manager) holds.
// The daemon process reads whichever exists — the operator does not configure
// this, and there is nothing to mount when the process runs on the host.
var CAPaths = []string{
	"/var/lib/docker/swarm/ca.pem",
	"/var/lib/docker/swarm/certificates/swarm-root-ca.crt",
}

// NodeCertPaths are the node certificate and key Docker provisions for this
// node. A worker presents them; nothing here mints, rotates or stores a second
// credential.
var (
	NodeCertPaths = []string{"/var/lib/docker/swarm/certificates/swarm-node.crt"}
	NodeKeyPaths  = []string{"/var/lib/docker/swarm/certificates/swarm-node.key"}
)

// NodeLister is the live swarm node list — the authority for "is this
// certificate's CN still a node of this cluster".
type NodeLister interface {
	NodeList(ctx context.Context) ([]runtime.Node, error)
}

// Node is the identity a request carries: derived from the client certificate,
// never from the request body or the query string.
type Node struct {
	// ID is the swarm node ID (the certificate's CN).
	ID string
	// Hostname is the node's hostname from the live node list.
	Hostname string
	Role     string
}

// contextKey is unexported so no other package can forge an identity.
type contextKey struct{}

// FromContext returns the authenticated node. ok is false when the handler ran
// without this middleware, which is a programming error, not a 403.
func FromContext(ctx context.Context) (Node, bool) {
	n, ok := ctx.Value(contextKey{}).(Node)
	return n, ok
}

// ContextWithNode installs an authenticated identity.
//
// The middleware does this itself; the export exists so a handler wired behind a
// DIFFERENT front door (or a test) produces the same identity shape. It must never
// be called from a path that has not verified a certificate: it is the "identity
// established" step, not the check.
func ContextWithNode(ctx context.Context, n Node) context.Context {
	return context.WithValue(ctx, contextKey{}, n)
}

// Verifier checks the four rules in order. Every field must be wired: a nil
// Nodes or SwarmID refuses everything, because a half-configured verifier must
// fail closed.
type Verifier struct {
	// CA is the swarm root CA pool.
	CA *x509.CertPool
	// Nodes is the live node list (the CN allowlist and the revocation path).
	Nodes NodeLister
	// SwarmID returns this cluster's swarm ID. Read from `docker info` on a
	// manager; the caller passes a function so a leadership change is picked up.
	SwarmID func(ctx context.Context) (string, error)
	// Now is the clock (tests).
	Now func() time.Time
	Log zerolog.Logger
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// Middleware authenticates the peer from its client certificate and puts the
// resolved node in the request context. A failure is a bare 403 with one short
// reason: the caller is another node of this cluster, so it may learn WHICH rule
// failed, but never which node IDs or swarm ID would have been accepted.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		node, reason := v.Authenticate(r)
		if reason != "" {
			v.deny(w, r, reason)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, node)))
	})
}

// deny answers 403 with a short reason and logs the detail (never a certificate
// body, never a key).
func (v *Verifier) deny(w http.ResponseWriter, r *http.Request, reason string) {
	v.Log.Warn().Str("rule", reason).Str("path", r.URL.Path).
		Msg("swarm tls: refused a request without a live swarm node certificate")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":  "forbidden",
		"reason": reason,
	})
}

// Authenticate applies the four rules and returns the identity, or a short
// reason string. It is exported so the same rules can be tested (and reused by
// the client half) without an HTTP round trip.
func (v *Verifier) Authenticate(r *http.Request) (Node, string) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return Node{}, "no client certificate"
	}
	leaf := r.TLS.PeerCertificates[0]
	if len(r.TLS.PeerCertificates) > 1 {
		// Only the leaf is interesting; Docker does not chain node certs, and a
		// caller-supplied extra certificate must not influence the checks.
		leaf = r.TLS.PeerCertificates[0]
	}

	// 1. Chain to the swarm CA. Done again here (the TLS handshake already
	//    required and verified a client certificate) so the rule set is one
	//    function, testable without a socket, and so a future listener
	//    configuration change cannot silently drop it.
	if err := VerifyChain(v.CA, leaf, r.TLS.PeerCertificates[1:], v.now()); err != nil {
		// Go's verifier checks validity as part of the chain walk, so an expired
		// certificate surfaces here. Name it for what it is: the operator's fix
		// differs (Docker renews node certificates; it does not re-issue a CA).
		var invalid x509.CertificateInvalidError
		if errors.As(err, &invalid) && invalid.Reason == x509.Expired {
			return Node{}, "client certificate is expired"
		}
		return Node{}, "certificate does not chain to the swarm CA"
	}

	// 2. Not expired. TLS already refuses an expired certificate at the
	//    handshake; the check stays so "expired is rejected" is a property of
	//    this function rather than of a listener flag someone can flip.
	if now := v.now(); now.After(leaf.NotAfter) {
		return Node{}, "client certificate is expired"
	} else if now.Before(leaf.NotBefore) {
		return Node{}, "client certificate is not valid yet"
	}

	// 3. This cluster's swarm ID as the certificate's organisation. A node of a
	//    different swarm has a chain signed by ITS ca.pem, which is not in this
	//    pool either — this rule is the second lock, and it is the one that
	//    survives a CA file mix-up.
	if v.SwarmID == nil {
		return Node{}, "swarm id unavailable"
	}
	want, err := v.SwarmID(r.Context())
	if err != nil || strings.TrimSpace(want) == "" {
		return Node{}, "swarm id unavailable"
	}
	if !HasOrg(leaf, want) {
		return Node{}, "certificate is not from this swarm"
	}

	// 4. CN is a node ID in the LIVE node list. This is the revocation path:
	//    `docker node rm` (or a node that was replaced) no longer appears, so
	//    its certificate stops working here without any token to rotate.
	if v.Nodes == nil {
		return Node{}, "node list unavailable"
	}
	list, err := v.Nodes.NodeList(r.Context())
	if err != nil {
		return Node{}, "node list unavailable"
	}
	cn := strings.TrimSpace(leaf.Subject.CommonName)
	for _, n := range list {
		if n.ID == "" || !strings.EqualFold(n.ID, cn) {
			continue
		}
		return Node{ID: n.ID, Hostname: n.Hostname, Role: n.Role}, ""
	}
	return Node{}, "certificate is not a live swarm node"
}

// VerifyChain checks that leaf chains to roots, is not expired as of now, and is
// valid for client authentication.
func VerifyChain(roots *x509.CertPool, leaf *x509.Certificate, intermediates []*x509.Certificate, now time.Time) error {
	if roots == nil {
		return fmt.Errorf("no CA pool")
	}
	if leaf == nil {
		return fmt.Errorf("no certificate")
	}
	opts := x509.VerifyOptions{
		Roots:         roots,
		Intermediates: x509.NewCertPool(),
		CurrentTime:   now,
		// A node certificate is a CLIENT certificate when this side is the
		// server, and a SERVER certificate when this side dials. Both are the
		// same Docker certificate, so both usages are accepted.
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}
	for _, ic := range intermediates {
		if ic != nil {
			opts.Intermediates.AddCert(ic)
		}
	}
	_, err := leaf.Verify(opts)
	return err
}

// HasOrg reports whether the certificate carries the given organisation.
// Docker puts the swarm (Raft) cluster ID there, which is what makes the check
// meaningful: two swarms have different IDs, and a node cannot change its own.
func HasOrg(cert *x509.Certificate, org string) bool {
	org = strings.TrimSpace(org)
	if cert == nil || org == "" {
		return false
	}
	for _, o := range cert.Subject.Organization {
		if strings.EqualFold(strings.TrimSpace(o), org) {
			return true
		}
	}
	return false
}

// SwarmIDOf returns the swarm ID a certificate claims (its single O), or "".
// The client uses it to check the SERVER's certificate against the swarm it
// already belongs to — a worker cannot read `docker info`'s cluster block, but
// its own certificate carries the same organisation.
func SwarmIDOf(cert *x509.Certificate) string {
	if cert == nil || len(cert.Subject.Organization) == 0 {
		return ""
	}
	return strings.TrimSpace(cert.Subject.Organization[0])
}

// LoadCA reads the first readable swarm CA from paths (default CAPaths) and
// returns the parsed pool and the path it used. The caller logs the path, so a
// fleet running an older Docker layout is visible rather than mysterious.
func LoadCA(paths ...string) (*x509.CertPool, string, error) {
	if len(paths) == 0 {
		paths = CAPaths
	}
	var tried []string
	for _, p := range paths {
		pem, err := os.ReadFile(p)
		if err != nil {
			tried = append(tried, p)
			continue
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			tried = append(tried, p+" (unparsable)")
			continue
		}
		return pool, p, nil
	}
	return nil, "", fmt.Errorf("no swarm CA found (tried %s)", strings.Join(tried, ", "))
}

// FirstFile returns the first path that exists, or "".
func FirstFile(paths []string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// ServeConfig describes the dedicated mTLS listener.
type ServeConfig struct {
	// Addr is the bind address. The caller passes the node's swarm data-path
	// address (from `docker info`), so this listener is never on a public host
	// port; 127.0.0.1 is the fallback when the node has no swarm address.
	Addr string
	// CertFile/KeyFile are this node's swarm certificate: the leader proves it
	// is a swarm node too, so a worker's dialer can apply the same rules.
	CertFile string
	KeyFile  string
	// CA is the swarm root CA pool. Every client certificate must chain to it.
	CA *x509.CertPool
	// CAFile is where that pool was read from, for the startup log.
	CAFile  string
	Handler http.Handler
	Log     zerolog.Logger
}

// Serve runs the mTLS listener until ctx is cancelled.
//
// ClientAuth is RequireAndVerifyClientCert: a caller without a certificate
// signed by the swarm CA never reaches the handler. That is the primary gate;
// the Verifier middleware then applies the cluster-scoped rules (O, live CN,
// expiry) that TLS cannot see.
func Serve(ctx context.Context, cfg ServeConfig) error {
	if cfg.Addr == "" {
		return fmt.Errorf("swarmtls: no listen address")
	}
	if cfg.CA == nil {
		return fmt.Errorf("swarmtls: no swarm CA pool")
	}
	pair, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("swarmtls: load node certificate: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    cfg.CA,
		MinVersion:   tls.VersionTLS12,
	}
	srv := &http.Server{
		Handler:           cfg.Handler,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		// One request, one answer: no keep-alive across calls and no idle
		// connection left holding a goroutine on a worker.
		IdleTimeout: 30 * time.Second,
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("swarmtls: listen on %s: %w", cfg.Addr, err)
	}
	cfg.Log.Info().Str("addr", cfg.Addr).Str("ca", cfg.CAFile).
		Msg("swarm tls: serving the runner assignment endpoint to swarm nodes")
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := srv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed && !strings.Contains(err.Error(), "use of closed") {
		return fmt.Errorf("swarmtls: serve on %s: %w", cfg.Addr, err)
	}
	return nil
}

// ClientTLS builds the dialer's configuration: this node's swarm certificate,
// and a verifier for the SERVER's certificate that applies the same rules a
// server applies to a client.
//
// Hostname verification is replaced, not disabled: a Docker node certificate
// carries DNS:swarm-manager / DNS:<node id> and no IP SAN, so the standard
// check cannot pass against 10.7.224.11. VerifyPeerCertificate keeps the
// guarantee that matters (chain to the swarm CA + the cluster's O) and adds the
// CN-is-a-live-node rule, so a worker cannot be redirected to an arbitrary
// TLS endpoint that happens to hold a certificate from another swarm.
type ClientConfig struct {
	CertFile string
	KeyFile  string
	CA       *x509.CertPool
	// SwarmID is the swarm this client belongs to (from its own certificate).
	SwarmID string
	// Nodes is the live node list, used only on a manager (a worker cannot list
	// nodes, so it skips the CN check and relies on CA + O).
	Nodes NodeLister
}

// ClientTLS returns the tls.Config for the worker's assignment fetch. An error
// means "this node cannot present a swarm certificate" — the caller logs one
// WARN and skips the poll; it must never fall back to an unauthenticated call.
func ClientTLS(cfg ClientConfig) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load swarm node certificate: %w", err)
	}
	if cfg.CA == nil {
		return nil, fmt.Errorf("no swarm CA pool")
	}
	c := &tls.Config{
		Certificates: []tls.Certificate{pair},
		MinVersion:   tls.VersionTLS12,
		// Verification happens in VerifyPeerCertificate below; see the type
		// comment for why the hostname check is replaced rather than disabled.
		InsecureSkipVerify: true,
	}
	c.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return fmt.Errorf("server presented no certificate")
		}
		leaf, err := x509.ParseCertificate(raw[0])
		if err != nil {
			return fmt.Errorf("parse server certificate: %w", err)
		}
		var intermediates []*x509.Certificate
		for _, der := range raw[1:] {
			if ic, err := x509.ParseCertificate(der); err == nil {
				intermediates = append(intermediates, ic)
			}
		}
		if err := VerifyChain(cfg.CA, leaf, intermediates, time.Now()); err != nil {
			return fmt.Errorf("server certificate does not chain to the swarm CA: %w", err)
		}
		if want := strings.TrimSpace(cfg.SwarmID); want != "" && !HasOrg(leaf, want) {
			return fmt.Errorf("server certificate is not from this swarm")
		}
		return nil
	}
	return c, nil
}
