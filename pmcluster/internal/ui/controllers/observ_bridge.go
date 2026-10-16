package controllers

import (
	"encoding/json"
	"html/template"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// ObservBridge serves the observability single-sign-on bootstrap page on the
// observ.<domain> origin, reached through the Traefik `sso-bridge` router
// (priority 1000, so it wins over the catch-all observability router).
//
// Why this exists at all: the SSO gate authenticates the OPERATOR, but the
// backend's own web app decides whether it is logged in from state it keeps in
// the BROWSER. OpenObserve needed localStorage["userInfo"]/["currentuser"];
// Parseable needs localStorage["authToken"] — `isAuthenticated` is literally
// `!!localStorage.getItem("authToken")`, so an operator who passes SSO with no
// such key is shown the sign-in form. That state cannot be produced from the
// edge (it is per-origin browser storage), so this page writes it and redirects.
//
// The value written is the SAME managed credential the Traefik
// `observability-auto-auth` middleware injects on every request, rendered into
// this service's environment from the same RenderInput field. One login for the
// collector, the UI and the operator.
//
// Like the console's own routes, this page cannot be gated by a console session
// — the browser has no edge session when it lands here. The Traefik sso-auth (or
// admin-auth) middleware upstream is the real gate; this handler re-checks the
// identity headers that middleware supplies.
type ObservBridge struct{ *Controller }

// observBasicAuthEnv carries base64(<admin user>:<admin password>) — the exact
// value the observability router's injection middleware sends.
const observBasicAuthEnv = "OBSERVABILITY_BASIC_AUTH"

// Page renders the bootstrap document.
func (b ObservBridge) Page(g *gin.Context) {
	basicAuth := os.Getenv(observBasicAuthEnv)

	if !observBridgeGate(g.Request) {
		g.Header("Cache-Control", "no-store")
		g.Status(http.StatusForbidden)
		_, _ = g.Writer.Write([]byte("forbidden\n"))
		return
	}

	g.Header("Content-Type", "text/html; charset=utf-8")
	g.Header("Cache-Control", "no-store")
	g.Status(http.StatusOK)

	// JSON-encode so the value is a well-formed JS string literal. template.JS
	// then embeds it without html/template escaping the quotes.
	encoded, err := json.Marshal(basicAuth)
	if err != nil {
		encoded = []byte(`""`)
	}

	// The page PROVES the credential before trusting it, rather than assuming the
	// shape the SPA expects. Its API client builds `Basic <btoa(user:pass)>`, so
	// the stored value could reasonably be either that whole header or the bare
	// base64 — and guessing wrong means a silent 401 on every request. So try the
	// candidates against a real endpoint and store the one the server accepts.
	js := template.JS(`
const managed = ` + string(encoded) + `;
const bare = managed.replace(/^Basic\s+/i, "");
const candidates = [managed, "Basic " + bare, bare];

async function verify(value) {
  try {
    const r = await fetch("/api/v1/about", { headers: { Authorization: value } });
    return r.ok;
  } catch (_) {
    return false;
  }
}

(async () => {
  let chosen = managed;
  for (const c of candidates) {
    if (await verify(c)) { chosen = c; break; }
  }
  localStorage.setItem("authToken", chosen);
  location.replace("/");
})();
`)

	_, _ = g.Writer.Write([]byte(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Signing you in…</title>
<style>
body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0b0f14;color:#dbe2ea;font-family:system-ui,sans-serif}
p{opacity:.8;animation:pulse 1.4s ease-in-out infinite}
@keyframes pulse{0%,100%{opacity:.35}50%{opacity:.9}}
</style>
</head>
<body>
<p>Signing you in to the observability console…</p>
<script>` + string(js) + `</script>
</body>
</html>
`))
}

// observBridgeGate reports whether the request may be served. The page hands the
// managed admin credential to whoever loads it, so it must only run for a caller
// that already passed the gate upstream.
//
// When SSO is enabled the oauth2-proxy forwardAuth supplies X-Forwarded-User /
// X-Forwarded-Email (see the sso-auth middleware's authResponseHeaders) — their
// presence is proof the request came through it. When SSO is disabled Traefik's
// admin-auth still applies basic auth, and the shipped non-SSO flow must keep
// working, so any Basic authorization is accepted.
func observBridgeGate(r *http.Request) bool {
	if user := r.Header.Get("X-Forwarded-User"); user != "" {
		return true
	}
	if email := r.Header.Get("X-Forwarded-Email"); email != "" {
		return true
	}
	scheme, _, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	return strings.EqualFold(scheme, "Basic")
}
