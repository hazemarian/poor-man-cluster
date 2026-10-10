package controllers

import (
	"context"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// SSOBridge serves the OpenObserve single-sign-on bootstrap page on the
// observ.<domain> origin.
//
// OpenObserve's SPA decides login state from localStorage["userInfo"] on every
// navigation — the auth_tokens cookie (set by the openobserve-auto-auth
// Traefik middleware) only authenticates the API calls. A user who passes the
// GitHub SSO gate but has no entry under that key is therefore always bounced
// to /web/login. Since OSS OpenObserve has no trusted-proxy / forwarded-
// identity mode, the only way to establish the session client-side is to write
// the exact localStorage envelope the SPA expects.
//
// This page replicates the shape the SPA's own onSignIn flow produces:
//
//	userInfo   = btoa(encodeURIComponent(json))  (Iv())
//	currentuser = raw JSON of the same object     (sd())
//
// The user object includes a pgdata:{} key — when it is present, the boot
// hook takes the direct-login path (dispatch login + getDefaultOrganization)
// and SKIPS the VerifyAndCreateUser() → /api/users/verifyuser/{email} call,
// which 404s on this server. After writing the keys the page bounces to
// /web/, where the SPA finds the session and stays logged in.
//
// The page is served by the edge console (which shares the Traefik overlay
// with OpenObserve), so its JS runs on the observ origin and the keys land in
// the right localStorage scope.
type SSOBridge struct{ *Controller }

// ooSessionCookie is the cookie OpenObserve sets on a successful login (its
// openobserve-auto-auth middleware mints it after the SSO/forward-auth gate).
const ooSessionCookie = "auth_tokens"

// ssoBridgeGate reports whether a request to /sso-bridge may proceed. When SSO
// is disabled the endpoint is ungated at the console (the upstream Traefik
// admin-auth basic-auth middleware still protects it, and the shipped non-SSO
// flow must keep working). When SSO is enabled the request must carry the
// OpenObserve session cookie (auth_tokens) or a Basic Authorization header
// (the root admin's basic auth — verified upstream by Traefik's gate).
func ssoBridgeGate(r *http.Request, ssoEnabled bool) bool {
	if !ssoEnabled {
		return true
	}
	if c, err := r.Cookie(ooSessionCookie); err == nil && c.Value != "" {
		return true
	}
	return hasBasicAuth(r)
}

// hasBasicAuth reports whether the request carries a Basic Authorization
// header (any credentials — the console gate only distinguishes "some auth
// signal" from "anonymous"; Traefik's middleware is the real authenticator).
func hasBasicAuth(r *http.Request) bool {
	scheme, _, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	return strings.EqualFold(scheme, "Basic")
}

// Page renders the bootstrap HTML document.
func (b SSOBridge) Page(g *gin.Context) {
	ctx, cancel := context.WithTimeout(g.Request.Context(), 2*time.Second)
	defer cancel()

	email := "admin@" + b.Domain
	// Best effort: the real OpenObserve admin email from cluster settings.
	// Fall back to the default admin@<domain> derived by the setup wizard.
	ssoEnabled := false
	if s, err := b.API.ClusterSettings(ctx); err == nil {
		if v := s["oo_admin_email"]; v != "" {
			email = v
		}
		ssoEnabled = s["sso_enabled"] == "true"
	}

	// Defense-in-depth: when SSO is enabled, refuse anonymous requests — the
	// bridge writes a root OpenObserve session, so it must only run for a
	// caller that already passed the SSO (or root) gate.
	if !ssoBridgeGate(g.Request, ssoEnabled) {
		g.Header("Cache-Control", "no-store")
		g.Status(http.StatusForbidden)
		_, _ = g.Writer.Write([]byte("forbidden\n"))
		return
	}

	g.Header("Content-Type", "text/html; charset=utf-8")
	g.Header("Cache-Control", "no-store")
	g.Status(http.StatusOK)

	// The user object mirrors what OO's login handler builds on success.
	// template.JS keeps the email safe to inline; the email is a config value
	// (admin@<domain> or the oo_admin_email setting) — no user input.
	js := template.JS(`
const email = "` + template.JS(email) + `";
const now = Math.floor(Date.now() / 1000);
// O is the user object written into OpenObserve's localStorage session.
const O = {
  given_name: email,
  auth_time: now,
  name: email,
  exp: now + 2592000,
  family_name: "",
  email: email,
  role: "root",
  pgdata: {}
};
// Iv is OpenObserve's base64url userInfo encoder (btoa of percent-encoded JSON).
const Iv = s => btoa(encodeURIComponent(s).replace(/%([0-9A-F]{2})/g, (_, h) => String.fromCharCode(parseInt("0x" + h))));
localStorage.setItem("userInfo", Iv(JSON.stringify(O)));
localStorage.setItem("currentuser", JSON.stringify(O));
location.replace("/web/");
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
<p>Signing you in to OpenObserve…</p>
<script>` + string(js) + `</script>
</body>
</html>
`))
}
