package pullcred

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

// TokenSource mints one pull credential. The edge implements it by asking the
// daemon that holds the GitHub App key (the daemon is the only process that can
// sign the App JWT); tests implement it directly.
type TokenSource interface {
	PullToken(ctx context.Context, server string) (Token, error)
}

// Server is the credential listener's request surface. It is mounted on its own
// http.Server, with its own mux, and nothing else: the console handler tree is
// not reachable from here, and this path is not reachable from the console.
type Server struct {
	Verifier *Verifier
	Tokens   TokenSource
	Log      *zerolog.Logger
}

// Handler returns the credential route's mux. Only TokenPath is registered; any
// other path is a 404 from the mux itself, which is what keeps this listener
// from being a second door into anything else.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(TokenPath, s.handleToken)
	return mux
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		s.fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// The TLS handshake already refused a missing/foreign/unlisted certificate
	// (see Verifier.TLSConfig). This second check makes the handler safe on its
	// own — if it is ever mounted behind something that terminated TLS, or a
	// future refactor drops VerifyConnection, the rule still applies here.
	leaf := peerLeaf(r)
	if err := s.verifier().Verify(r.Context(), leaf); err != nil {
		s.logReject(r, err)
		s.fail(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "read body")
		return
	}
	if len(body) > MaxBodyBytes {
		s.fail(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		s.fail(w, http.StatusBadRequest, "body must be JSON")
		return
	}
	if req.Server != Registry {
		// One registry, named in the answer: the installation token is a GHCR
		// credential and answering for anything else would hand it to a caller
		// that asked for a different host.
		s.fail(w, http.StatusBadRequest, fmt.Sprintf("unsupported registry %q (only %s is served)", req.Server, Registry))
		return
	}
	if s.Tokens == nil {
		s.fail(w, http.StatusServiceUnavailable, "no token source is configured")
		return
	}

	tok, err := s.Tokens.PullToken(r.Context(), req.Server)
	if err != nil {
		// The pull fails either way; the cause in the body is what turns
		// "unauthorized" into "the App key was rejected". Only a node of this
		// swarm can read it.
		s.log(zerolog.ErrorLevel, func(e *zerolog.Event) *zerolog.Event {
			return e.Err(err).Str("node", leaf.Subject.CommonName)
		}, "pull credential mint failed")
		s.fail(w, http.StatusBadGateway, "mint installation token: "+err.Error())
		return
	}
	if tok.Secret == "" {
		s.fail(w, http.StatusBadGateway, "the token source returned an empty token")
		return
	}
	if tok.Username == "" {
		tok.Username = Username
	}
	if tok.ExpiresAt.IsZero() {
		tok.ExpiresAt = time.Now().Add(time.Hour)
	}

	// The token is the response body and nothing else: not a header, not a log
	// line, not a cache entry.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(tok)

	s.log(zerolog.InfoLevel, func(e *zerolog.Event) *zerolog.Event {
		return e.Str("node", leaf.Subject.CommonName).Time("expires_at", tok.ExpiresAt.UTC())
	}, "pull credential issued")
}

func (s *Server) verifier() *Verifier {
	if s.Verifier != nil {
		return s.Verifier
	}
	// No verifier is a configuration bug, and the safe interpretation is "no
	// certificate is acceptable" — ErrNoSwarmID is exactly that answer.
	return &Verifier{}
}

// fail writes one error object. It never includes the cause of an auth failure
// (a caller must not be able to tell a foreign CA from an unlisted node) and
// never a token.
func (s *Server) fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *Server) logReject(r *http.Request, err error) {
	cn := ""
	if leaf := peerLeaf(r); leaf != nil {
		cn = leaf.Subject.CommonName
	}
	s.log(zerolog.WarnLevel, func(e *zerolog.Event) *zerolog.Event {
		return e.Err(err).Str("node", cn).Str("remote", remoteHost(r))
	}, "pull credential request rejected")
}

func (s *Server) log(level zerolog.Level, fields func(*zerolog.Event) *zerolog.Event, msg string) {
	if s.Log == nil {
		return
	}
	fields(s.Log.WithLevel(level)).Msg(msg)
}

func remoteHost(r *http.Request) string {
	if r == nil {
		return ""
	}
	return r.RemoteAddr
}

// peerLeaf is the client certificate the connection authenticated with, or nil
// when there is no TLS peer at all.
func peerLeaf(r *http.Request) *x509.Certificate {
	if r == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return nil
	}
	return r.TLS.PeerCertificates[0]
}
