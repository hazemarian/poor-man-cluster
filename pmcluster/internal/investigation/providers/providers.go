// Package providers speaks the wire protocols of the major model vendors.
// Every protocol makes one nonstreaming, no-redirect request with a bounded
// reply and returns a citation-checked verdict.
package providers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/investigation"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/investigation/compatible"
)

// Router dispatches a policy to the client for its protocol.
type Router struct{ HTTP *http.Client }

// Run implements investigation.Provider.
func (r Router) Run(ctx context.Context, p investigation.Policy, key string, in investigation.Input) (investigation.Verdict, *int64, error) {
	if key == "" {
		return investigation.Verdict{}, nil, errors.New("provider key is not configured")
	}
	switch p.Proto() {
	case investigation.ProtocolAnthropic:
		return r.anthropic(ctx, p, key, in)
	case investigation.ProtocolGemini:
		return r.gemini(ctx, p, key, in)
	default:
		return compatible.Client{HTTP: r.HTTP, Headers: routingHeaders(p.BaseURL)}.Run(ctx, p, key, in)
	}
}

// encodeInput serialises the evidence and enforces the per-call input allowance.
func encodeInput(p investigation.Policy, in investigation.Input) (string, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return "", errors.New("encode evidence")
	}
	if int64(len(b)+len(investigation.SystemPrompt)+1024) > p.MaxInputTokens {
		return "", errors.New("input exceeds configured allowance")
	}
	return string(b), nil
}

// modelName is the only policy value placed in a URL path (Gemini), so it is
// restricted to the characters real model ids use.
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func trimBase(base string) string { return strings.TrimRight(base, "/") }

// routingHeaders returns provider-specific headers that carry no credential.
// OpenCode Go rejects a request without x-opencode-session, which it uses for
// routing and prompt caching. Each investigation is one conversation, so a
// fresh random id per call is correct; it identifies nothing about the
// cluster. The header is sent only to opencode.ai so no other provider
// receives it.
func routingHeaders(base string) map[string]string {
	u, err := url.Parse(base)
	if err != nil {
		return nil
	}
	host := u.Hostname()
	if host != "opencode.ai" && !strings.HasSuffix(host, ".opencode.ai") {
		return nil
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil
	}
	return map[string]string{"x-opencode-session": hex.EncodeToString(b)}
}

func withRouting(h map[string]string, base string) map[string]string {
	for k, v := range routingHeaders(base) {
		h[k] = v
	}
	return h
}
