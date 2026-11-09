package githubapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"golang.org/x/time/rate"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// The token broker: POST /api/github/token.
//
// A GitHub Actions workflow in an org repo calls this instead of holding
// `secrets.GH_PAT`. The caller proves itself with a per-repo HMAC shared
// secret; the daemon mints one installation token and returns it with its
// expiry. The token is valid for about an hour, so a compromised workflow log
// leaks nothing durable.
//
// Why HMAC and not the daemon bearer token:
//
//   - A bearer token is full control-plane access (deploy, read secrets, exec).
//     Putting one in every repo turns nine CI configs into nine cluster-admin
//     credentials; the whole reason for this endpoint is to stop spreading a
//     long-lived secret.
//   - The webhook receiver already authenticates CI with exactly this scheme
//     (HMAC-SHA256 over timestamp+body, sha256=<hex>, 5-minute skew), so the
//     org's pipelines and its deploy hooks use one pattern, not two.
//   - The broker secret is scoped to one repo NAME and can only mint; revoking
//     it (delete the row) does not touch the App key or any other repo.
//
// The endpoint cannot narrow permissions: GitHub issues installation tokens at
// the App's granted permissions. The App's own configuration is the ceiling,
// and the console/doctor report it.

// Broker header and body limits. The names match the webhook receiver so one
// openssl invocation works for both.
const (
	// BrokerSignatureHeader is "sha256=<lowercase-hex>".
	BrokerSignatureHeader = "X-Pmcluster-Signature"
	// BrokerTimestampHeader is unix seconds, part of the signed material.
	BrokerTimestampHeader = "X-Pmcluster-Timestamp"
	// BrokerRepoHeader names which repo's shared secret signs the request.
	BrokerRepoHeader = "X-Pmcluster-Repo"

	// BrokerMaxClockSkew is how far a request timestamp may drift.
	BrokerMaxClockSkew = 5 * time.Minute
	// BrokerMaxBodyBytes bounds the body; workflows send none, but the HMAC
	// covers whatever is sent so the limit is part of the contract.
	BrokerMaxBodyBytes = 4 << 10
)

// Default broker rate limit: five mints immediately, then one every six seconds
// (~10/minute), per repo. A workflow mints once per job, so this is generous;
// it exists to bound damage from a leaked broker secret, not to pace CI.
const (
	DefaultBrokerRatePerMinute = 10
	DefaultBrokerBurst         = 5
)

// SecretReader supplies the decrypted HMAC secret for one repo. Local
// implements it; tests use a map.
type SecretReader interface {
	BrokerSecret(ctx context.Context, repo string) ([]byte, error)
}

// SecretRecorder records a successful mint. Optional.
type SecretRecorder interface {
	MarkBrokerSecretUsed(ctx context.Context, repo string) error
}

// Broker serves POST /api/github/token.
type Broker struct {
	Secrets SecretReader
	Minter  TokenSource
	// Now overrides time.Now (tests).
	Now func() time.Time
	// RatePerMinute and Burst tune the per-repo limiter. Zero means the
	// defaults above; a negative RatePerMinute disables limiting (tests only).
	RatePerMinute int
	Burst         int
	Log           *zerolog.Logger

	mu       sync.Mutex
	limiters map[string]*rate.Limiter
}

// Mount registers the broker route. The caller MUST mount it OUTSIDE the
// Bearer-protected /api group: HMAC IS the auth, and a workflow has no bearer
// token. chi matches this exact path before the /api subtree.
func (b *Broker) Mount(r chi.Router) {
	r.Post("/api/github/token", b.serve)
}

func (b *Broker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// allow applies the per-repo token bucket.
func (b *Broker) allow(repo string) bool {
	if b.RatePerMinute < 0 {
		return true
	}
	perMinute, burst := b.RatePerMinute, b.Burst
	if perMinute == 0 {
		perMinute = DefaultBrokerRatePerMinute
	}
	if burst <= 0 {
		burst = DefaultBrokerBurst
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limiters == nil {
		b.limiters = map[string]*rate.Limiter{}
	}
	lim, ok := b.limiters[repo]
	if !ok {
		lim = rate.NewLimiter(rate.Limit(float64(perMinute)/60.0), burst)
		b.limiters[repo] = lim
	}
	return lim.Allow()
}

func (b *Broker) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, BrokerMaxBodyBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}
	if len(body) > BrokerMaxBodyBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "body too large"})
		return
	}

	repo := strings.TrimSpace(r.Header.Get(BrokerRepoHeader))
	if err := b.verify(r.Context(), repo, r.Header.Get(BrokerTimestampHeader), r.Header.Get(BrokerSignatureHeader), body); err != nil {
		// One status for every auth failure so a caller cannot distinguish
		// "unknown repo" from "bad signature".
		b.logEvent(repo, "unauthorized")
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}

	if !b.allow(repo) {
		b.logEvent(repo, "rate_limited")
		w.Header().Set("Retry-After", "6")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "rate limit exceeded for this repository"})
		return
	}

	if b.Minter == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "github app not configured: token minting is disabled on this daemon"})
		return
	}
	tok, err := b.Minter.Token(r.Context())
	if err != nil {
		b.logEvent(repo, "mint_failed")
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": b.mintError(err)})
		return
	}

	if rec, ok := b.Secrets.(SecretRecorder); ok {
		// Best-effort: a history write must never fail a mint.
		_ = rec.MarkBrokerSecretUsed(r.Context(), repo)
	}
	b.logEvent(repo, "minted")
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      tok.Token,
		"expires_at": tok.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// verify checks the repo header, timestamp and HMAC in constant time. Any
// failure is an error; the caller turns all of them into one 401.
func (b *Broker) verify(ctx context.Context, repo, timestampHeader, sigHeader string, body []byte) error {
	if err := validateRepo(repo); err != nil {
		return err
	}
	ts, err := strconv.ParseInt(timestampHeader, 10, 64)
	if err != nil || ts <= 0 {
		return errors.New("invalid timestamp")
	}
	delta := b.now().Unix() - ts
	if delta < 0 {
		delta = -delta
	}
	if time.Duration(delta)*time.Second > BrokerMaxClockSkew {
		return errors.New("timestamp skew too large")
	}
	parsed, ok := strings.CutPrefix(sigHeader, "sha256=")
	if !ok {
		return errors.New("signature must be sha256=<hex>")
	}
	want, err := hex.DecodeString(parsed)
	if err != nil || len(want) != sha256.Size {
		return errors.New("signature must be 64 hex chars after sha256=")
	}
	if b.Secrets == nil {
		return errors.New("no secret source")
	}
	secret, err := b.Secrets.BrokerSecret(ctx, repo)
	if err != nil {
		// Unknown repo: no distinguishing message, and no HMAC work on a
		// secret we do not have.
		return err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = io.WriteString(mac, timestampHeader)
	mac.Write(body)
	if !hmac.Equal(want, mac.Sum(nil)) {
		return errors.New("signature mismatch")
	}
	return nil
}

// mintError renders a mint failure for the caller. The sentinel causes are
// preserved so an operator reading a workflow log sees "installation removed"
// rather than "502".
func (b *Broker) mintError(err error) string {
	switch {
	case errors.Is(err, ErrNoApp):
		return "github app not configured on this daemon"
	case errors.Is(err, ErrBadKey):
		return "the daemon's GitHub App key was rejected: " + err.Error()
	case errors.Is(err, ErrInstallationNotFound):
		return "the GitHub App installation is gone: " + err.Error()
	case errors.Is(err, ErrInsufficientPermission):
		return "the GitHub App lacks a required permission: " + err.Error()
	case errors.Is(err, store.ErrGitHubBrokerSecretNotFound):
		return "no broker secret for this repository"
	default:
		return "mint installation token: " + err.Error()
	}
}

// logEvent records the outcome without the secret, the signature, the JWT or
// the token — repo, outcome and nothing else.
func (b *Broker) logEvent(repo, outcome string) {
	if b.Log == nil {
		return
	}
	b.Log.Info().Str("repo", repo).Str("outcome", outcome).Msg("github token broker")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
