package cluster

import (
	"strings"
	"testing"
)

// The console router used to be declared as PathPrefix("/web"). Traefik matches
// a prefix on raw characters, not on path segments, so /webhook — the endpoint
// every application deploy posts to — also matched the console router and was
// dragged behind its admin-auth / oauth2-proxy gate: a 403 on every deploy,
// while the API router's negated prefix excluded the same path and left nothing
// for it to fall through to.
//
// A string assertion cannot see that bug: both rules look correct in isolation.
// These tests evaluate the rendered rules against real request paths, with the
// same semantics Traefik uses, so what gets asserted is the routing decision.

const tick = byte(96) // the backquote Traefik wraps literals in

type traefikRequest struct {
	host string
	path string
}

// ruleParser evaluates the subset of Traefik rule syntax the edge stack emits:
// Host(), PathPrefix(), Path(), negation, conjunction, disjunction and
// parentheses, with conjunction binding tighter than disjunction.
type ruleParser struct {
	src string
	pos int
}

func (p *ruleParser) skipSpace() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

func (p *ruleParser) accept(tok string) bool {
	p.skipSpace()
	if strings.HasPrefix(p.src[p.pos:], tok) {
		p.pos += len(tok)
		return true
	}
	return false
}

func (p *ruleParser) literal() string {
	p.skipSpace()
	if p.pos >= len(p.src) || p.src[p.pos] != tick {
		return ""
	}
	end := strings.IndexByte(p.src[p.pos+1:], tick)
	if end < 0 {
		return ""
	}
	val := p.src[p.pos+1 : p.pos+1+end]
	p.pos += end + 2
	return val
}

func (p *ruleParser) or(r traefikRequest) bool {
	left := p.and(r)
	for p.accept("||") {
		right := p.and(r)
		left = left || right
	}
	return left
}

func (p *ruleParser) and(r traefikRequest) bool {
	left := p.unary(r)
	for p.accept("&&") {
		right := p.unary(r)
		left = left && right
	}
	return left
}

func (p *ruleParser) unary(r traefikRequest) bool {
	if p.accept("!") {
		return !p.unary(r)
	}
	return p.primary(r)
}

func (p *ruleParser) primary(r traefikRequest) bool {
	p.skipSpace()
	if p.accept("(") {
		val := p.or(r)
		p.accept(")")
		return val
	}
	start := p.pos
	for p.pos < len(p.src) && isIdentByte(p.src[p.pos]) {
		p.pos++
	}
	name := p.src[start:p.pos]
	p.accept("(")
	val := p.literal()
	p.accept(")")

	switch name {
	case "Host":
		return strings.EqualFold(r.host, val)
	case "PathPrefix":
		// Raw prefix match: the behaviour that made /webhook a console path.
		return strings.HasPrefix(r.path, val)
	case "Path":
		return r.path == val
	default:
		return false
	}
}

func isIdentByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func ruleMatches(rule string, r traefikRequest) bool {
	p := &ruleParser{src: rule}
	return p.or(r)
}

// edgeRouterRules renders the edge stack and returns router name to rule, with
// the compose DOMAIN placeholder resolved so the rules can be evaluated. The
// rule text is found by substring scan rather than a regular expression: the
// syntax is mostly backquotes and dots, which a pattern handles worse than this
// does.
func edgeRouterRules(t *testing.T) map[string]string {
	t.Helper()
	data, err := LoadComposeFile(StackEdge, RenderInput{Domain: "example.com", SSOEnabled: true})
	if err != nil {
		t.Fatalf("LoadComposeFile(StackEdge): %v", err)
	}
	rules := map[string]string{}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		line = strings.TrimSpace(strings.TrimPrefix(line, "- "))
		line = strings.Trim(line, "\"")
		for _, name := range []string{"pmcluster-web", "pmcluster-api"} {
			marker := "traefik.http.routers." + name + ".rule="
			if idx := strings.Index(line, marker); idx >= 0 {
				rule := line[idx+len(marker):]
				rules[name] = strings.ReplaceAll(rule, "${DOMAIN}", "pmcluster.example.com")
			}
		}
	}
	if len(rules) != 2 {
		t.Fatalf("expected pmcluster-web and pmcluster-api rules, got %d: %v", len(rules), rules)
	}
	return rules
}

func TestEdgeRoutingSendsWebhooksToTheAPIRouter(t *testing.T) {
	rules := edgeRouterRules(t)
	host := "pmcluster.example.com"

	cases := []struct {
		path string
		want string
		why  string
	}{
		{"/webhook", "pmcluster-api", "the deploy webhook must never sit behind the console's auth gate"},
		{"/webhook/", "pmcluster-api", "trailing slash is the same endpoint"},
		{"/webhook/github", "pmcluster-api", "the payload path was the one returning 403"},
		{"/webhooks", "pmcluster-api", "a near-miss path must not be swallowed by the console rule"},
		{"/webxyz", "pmcluster-api", "anything starting with the four letters web is not the console"},
		{"/api/v1/cluster/info", "pmcluster-api", "the daemon API keeps its own Bearer auth"},
		{"/healthz", "pmcluster-api", "health probing stays unauthenticated"},
		{"/web", "pmcluster-web", "the console entry point redirects to /web/"},
		{"/web/", "pmcluster-web", "the console root"},
		{"/web/overview", "pmcluster-web", "console pages are auth-gated"},
		{"/web/static/base.css", "pmcluster-web", "console assets are auth-gated too"},
		{"/web/login", "pmcluster-web", "the sign-in screen is part of the console"},
	}

	for _, tc := range cases {
		got := ""
		for name, rule := range rules {
			if ruleMatches(rule, traefikRequest{host: host, path: tc.path}) {
				if got != "" {
					t.Errorf("%s matches BOTH %s and %s; Traefik would pick by priority, not by intent",
						tc.path, got, name)
				}
				got = name
			}
		}
		if got != tc.want {
			t.Errorf("%s routed to %q, want %q (%s)\n  web rule: %s\n  api rule: %s",
				tc.path, got, tc.want, tc.why, rules["pmcluster-web"], rules["pmcluster-api"])
		}
	}
}

// A rule that names the console host must not answer for another host, or the
// SSO callback host would start serving console paths.
func TestEdgeRoutingIgnoresOtherHosts(t *testing.T) {
	rules := edgeRouterRules(t)
	for _, path := range []string{"/web/overview", "/webhook"} {
		for name, rule := range rules {
			if ruleMatches(rule, traefikRequest{host: "elsewhere.example.com", path: path}) {
				t.Errorf("%s matched %s for an unrelated host", name, path)
			}
		}
	}
}
