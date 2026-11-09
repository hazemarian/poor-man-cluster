package githubapp

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// The pull-credential route: POST /api/github/pull-credential.
//
// This is the daemon half of the pull-time credential helper. The edge service
// publishes a credential listener on port 9443 (see internal/pullcred): a node's
// docker-credential-pmcluster presents its swarm certificate, the edge verifies
// it, and the edge asks THIS route — with the edge console's Bearer token — for
// the token. The daemon is the only process that can sign the App JWT, so it is
// the only process that can mint.
//
// Why the extra hop rather than minting in the edge: the App's private key
// lives encrypted in the daemon's store on the leader. Putting the key in the
// console container would give the process that renders HTML the ability to
// mint tokens for the whole installation, for no benefit — the console already
// holds a bearer token that can ask for one.
//
// It is Bearer-authenticated and operator-gated exactly like the app card next
// to it, so it adds no new trust: anyone who can call this could already read
// the app status and mint through /api/github/app/test.
//
// The response carries the token because that is the entire point; it is never
// logged, and the route is reachable only from the edge container's token.

// PullCredentialPath is the daemon route the edge's credential listener calls.
const PullCredentialPath = "/github/pull-credential"

// pullCredential mints one installation token for a pull credential.
func (h *HTTP) pullCredential(w http.ResponseWriter, r *http.Request) {
	if h.App == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "github app not configured: this daemon has no app key, so it cannot mint a pull credential (set one with `pmcluster github app set`)",
		})
		return
	}
	tok, err := h.App.Token(r.Context())
	if err != nil {
		if errors.Is(err, ErrNoApp) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "no GitHub App configured (set one with `pmcluster github app set`)",
			})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": mintErrorText(err),
		})
		return
	}
	if strings.TrimSpace(tok.Token) == "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "the GitHub App minted an empty token",
		})
		return
	}
	expires := tok.ExpiresAt
	if expires.IsZero() {
		expires = time.Now().Add(time.Hour)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username":   "x-access-token",
		"secret":     tok.Token,
		"expires_at": expires.UTC().Format(time.RFC3339),
	})
}

// mintErrorText turns the sentinel causes into the sentence an operator can act
// on. The same mapping the token broker uses, so a pull and a workflow report a
// rejected key the same way.
func mintErrorText(err error) string {
	switch {
	case errors.Is(err, ErrNoApp):
		return "no GitHub App configured (set one with `pmcluster github app set`)"
	case errors.Is(err, ErrBadKey):
		return "the daemon's GitHub App key was rejected: " + err.Error()
	case errors.Is(err, ErrInstallationNotFound):
		return "the GitHub App installation is gone: " + err.Error()
	case errors.Is(err, ErrInsufficientPermission):
		return "the GitHub App lacks a required permission: " + err.Error()
	case errors.Is(err, store.ErrGitHubAppNotConfigured):
		return "no GitHub App configured (set one with `pmcluster github app set`)"
	default:
		return "mint installation token: " + err.Error()
	}
}
