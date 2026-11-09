package pullcred

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The certificate rule: a client is a node of THIS swarm, or it is nothing.
//
// Docker Swarm already issues every node a certificate when it joins
// (/var/lib/docker/swarm/certificates/swarm-node.crt, key alongside it, signed
// by swarm-root-ca.crt). Its subject carries the two facts this route needs:
//
//	O  = the swarm's ID          — a certificate from another swarm fails here
//	CN = the swarm node's ID     — a certificate for a node that left fails here
//	OU = swarm-manager/swarm-worker (informational; both roles may pull)
//
// Verified against the live fleet on 2026-11-09:
//
//	subject=O=lm03ez1is3mlvt8zevoc2lfis, OU=swarm-manager, CN=n9xllyznqu52d6me0r6oeqhvv
//
// Note the CN is the NODE ID, not the hostname — the node list is compared by
// ID for that reason. (Docker writes these files under `certificates/`, not
// `certs/`; SwarmCertDir names the real directory.)
//
// The chain itself is verified by crypto/tls before any of this runs: a
// certificate signed by a foreign CA never reaches Verify. Verify is the
// authorization half — a valid swarm certificate for a DIFFERENT swarm, or for
// a node that is no longer a member, is a certificate this route must refuse.

// SwarmCertDir is where Docker keeps a node's swarm certificates.
const SwarmCertDir = "/var/lib/docker/swarm/certificates"

// Sentinel causes. Every rejection names what failed and what it saw, so an
// operator reading a pull error knows whether to rejoin the node or fix the CA.
var (
	// ErrNoPeerCertificate: the peer presented no client certificate at all.
	ErrNoPeerCertificate = errors.New("no client certificate presented")
	// ErrForeignSwarm: the certificate's O is not this swarm's ID.
	ErrForeignSwarm = errors.New("client certificate belongs to a different swarm")
	// ErrUnknownNode: the certificate's CN is not a current member of this swarm.
	ErrUnknownNode = errors.New("client certificate names a node that is not in this swarm")
	// ErrExpired: the certificate is outside its validity window.
	ErrExpired = errors.New("client certificate is expired or not yet valid")
	// ErrNoSwarmID: the listener was started without knowing its own swarm ID.
	// Fail closed: accepting every well-formed certificate because the check
	// could not run is the failure mode this whole file exists to prevent.
	ErrNoSwarmID = errors.New("this listener does not know its swarm ID, so it cannot authenticate a node")
)

// Verifier applies the node-certificate rule to a presented leaf certificate.
// It holds no state; NodeIDs is the fresh node list (the caller caches it).
type Verifier struct {
	// SwarmID is the swarm's own ID — the certificate's O must equal it.
	SwarmID string
	// SwarmIDFunc resolves the swarm ID when the listener does not know it up
	// front (the edge asks its daemon). Called only when SwarmID is empty. A
	// failed lookup is a refusal: "could not read the swarm ID" must never
	// become "every certificate matches".
	SwarmIDFunc func(ctx context.Context) (string, error)
	// NodeIDs returns the IDs of the nodes currently in the swarm. Required:
	// without it the CN check cannot pass, and failing closed is the point.
	NodeIDs func(ctx context.Context) ([]string, error)
	// Now overrides time.Now (tests).
	Now func() time.Time
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// Verify authorizes one presented certificate. An error is the whole answer:
// the route refuses with 401 and logs the reason without the certificate body.
func (v *Verifier) Verify(ctx context.Context, leaf *x509.Certificate) error {
	if leaf == nil {
		return ErrNoPeerCertificate
	}
	swarmID, err := v.swarmID(ctx)
	if err != nil {
		return err
	}
	if !strings.EqualFold(org(leaf), swarmID) {
		return fmt.Errorf("%w: certificate O=%q, this swarm is %q", ErrForeignSwarm, org(leaf), swarmID)
	}
	now := v.now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return fmt.Errorf("%w: valid %s to %s",
			ErrExpired, leaf.NotBefore.UTC().Format(time.RFC3339), leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	cn := strings.TrimSpace(leaf.Subject.CommonName)
	if cn == "" {
		return fmt.Errorf("%w: certificate has no CN", ErrUnknownNode)
	}
	if v.NodeIDs == nil {
		return fmt.Errorf("%w: no node list is configured", ErrUnknownNode)
	}
	ids, err := v.NodeIDs(ctx)
	if err != nil {
		// A node list we could not read is not a node list that contains the
		// caller: refuse, and say why, rather than verifying nothing.
		return fmt.Errorf("read the swarm node list: %w", err)
	}
	for _, id := range ids {
		if strings.TrimSpace(id) == cn {
			return nil
		}
	}
	return fmt.Errorf("%w: CN=%q (this swarm has %d node(s))", ErrUnknownNode, cn, len(ids))
}

// swarmID resolves this swarm's ID, or refuses. Empty on both paths is
// ErrNoSwarmID: a listener that cannot say which swarm it belongs to has no
// business accepting any certificate.
func (v *Verifier) swarmID(ctx context.Context) (string, error) {
	if v.SwarmID != "" {
		return v.SwarmID, nil
	}
	if v.SwarmIDFunc == nil {
		return "", ErrNoSwarmID
	}
	id, err := v.SwarmIDFunc(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoSwarmID, err)
	}
	if id = strings.TrimSpace(id); id == "" {
		return "", ErrNoSwarmID
	}
	return id, nil
}

// org returns a certificate's O (the first value; swarm certs carry exactly
// one).
func org(c *x509.Certificate) string {
	if len(c.Subject.Organization) == 0 {
		return ""
	}
	return c.Subject.Organization[0]
}

// TLSConfig builds the listener's TLS configuration: TLS 1.2+, a mandatory
// client certificate verified against the swarm root CA, and Verify applied to
// the chain-valid leaf. serverCert is what the listener presents; it carries no
// identity the client checks (see Remote.Get), but a TLS server without one
// cannot handshake at all.
//
// Order matters. crypto/tls first proves the certificate chains to caPEM — a
// self-signed or foreign-CA certificate fails the handshake and never reaches a
// handler. VerifyConnection then applies the swarm/role membership rule. Doing
// the chain check in VerifyConnection instead would mean re-implementing path
// building, and doing only the chain check would accept a certificate from any
// other swarm that happened to share this CA (it cannot, but the O check is
// free and states the intent).
func (v *Verifier) TLSConfig(caPEM []byte, serverCert tls.Certificate) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("swarm root CA: no PEM certificate found in %d byte(s)", len(caPEM))
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return ErrNoPeerCertificate
			}
			return v.Verify(context.Background(), cs.PeerCertificates[0])
		},
	}, nil
}
