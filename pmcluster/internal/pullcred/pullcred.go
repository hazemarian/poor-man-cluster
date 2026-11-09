// Package pullcred issues GHCR pull credentials to the cluster's own nodes.
//
// The problem it solves: a GitHub App installation token lives about an hour.
// The previous design minted one at daemon start and re-ran `docker login` on a
// 25-minute ticker, which left a credential in docker's config that is stale
// the moment the ticker stops — a manager restart, a paused daemon, a node
// that never ran the loop — and a stale credential fails a pull with the 401
// that reads like a missing image.
//
// So the credential is fetched when it is needed instead of being pushed on a
// timer. `docker-credential-pmcluster` is a docker credential helper: docker
// calls it at pull/deploy time, it asks this cluster for a token, and nothing
// durable is ever written anywhere.
//
// The requested resource is the edge service's credential listener, published
// with `mode: ingress` so the routing mesh answers on 127.0.0.1 on EVERY node
// and forwards to the one task that holds the minter. No node IP logic, no unix
// socket, no daemon proxy — the same shape as the OTLP receiver's 4318.
//
// Authentication is the Swarm node certificate Docker already issued to the
// node (O = this swarm's ID, CN = the node's ID, signed by the swarm root CA).
// There is deliberately no shared secret: the cluster already has a per-node
// identity, and inventing a second one would be one more thing to distribute
// and rotate. The certificate rule lives in Verifier; the wire format lives
// here; the listener in server.go and the helper in helper.go.
package pullcred

import (
	"time"
)

const (
	// TokenPath is the only route the credential listener serves. It lives on
	// its own listener and its own mux, so the console's session auth and
	// middleware are not in front of it (they would be a way to reach console
	// handlers with a node certificate) and this path can never be served by
	// the console (a node certificate would be a way around its auth).
	TokenPath = "/cred/v1/token"

	// Registry is the one host these credentials are for. The GitHub App's
	// installation token is a GHCR credential; answering for any other registry
	// would hand a GHCR token to a server that asked for docker.io.
	Registry = "ghcr.io"

	// Username is the account name GitHub expects with an installation token.
	Username = "x-access-token"

	// DefaultURL is where the helper dials. Port 9443 is published with
	// `mode: ingress` on the edge service, so this address answers on every
	// swarm node and the routing mesh forwards to the task that can mint.
	DefaultURL = "https://127.0.0.1:9443"

	// MaxBodyBytes bounds the request and the reply. The request is one
	// registry name; the reply is one token. Both are tiny, and a limit is what
	// keeps a hostile or half-broken peer from holding memory.
	MaxBodyBytes = 8 << 10

	// DefaultTimeout is the helper's whole budget. A pull that waits longer
	// than this should fail and be retried by Docker, not hang the pull.
	DefaultTimeout = 2 * time.Second
)

// Token is one minted credential. ExpiresAt is carried for humans (the doctor
// prints it) and for a caller that wants to cache; the token itself is opaque.
type Token struct {
	Username  string    `json:"username"`
	Secret    string    `json:"secret"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Request is the credential listener's request body.
type Request struct {
	Server string `json:"server"`
}
