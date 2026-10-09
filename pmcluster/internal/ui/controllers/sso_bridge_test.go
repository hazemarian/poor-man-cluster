package controllers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSSOBridgeGate covers the /sso-bridge gating logic: ungated when SSO is
// disabled; gated on the auth_tokens cookie or a Basic Authorization header
// when SSO is enabled.
func TestSSOBridgeGate(t *testing.T) {
	newReq := func(cookie string, authz string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "http://observ.example.com/sso-bridge", nil)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: ooSessionCookie, Value: cookie})
		}
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		return req
	}

	// SSO disabled → always allowed.
	if !ssoBridgeGate(newReq("", ""), false) {
		t.Error("SSO-disabled request without auth should be allowed")
	}

	// SSO enabled → anonymous is refused.
	if ssoBridgeGate(newReq("", ""), true) {
		t.Error("SSO-enabled anonymous request should be refused")
	}

	// SSO enabled → auth_tokens cookie passes.
	if !ssoBridgeGate(newReq("t0ken", ""), true) {
		t.Error("SSO-enabled request with auth_tokens should pass")
	}

	// SSO enabled → an empty auth_tokens cookie is not enough.
	if ssoBridgeGate(newReq("", ""), true) {
		t.Error("SSO-enabled request with empty auth_tokens should be refused")
	}

	// SSO enabled → root Basic authorization passes.
	if !ssoBridgeGate(newReq("", "Basic cm9vdDpwYXNz"), true) {
		t.Error("SSO-enabled request with Basic auth should pass")
	}

	// SSO enabled → a non-Basic scheme is not enough.
	if ssoBridgeGate(newReq("", "Bearer abc123"), true) {
		t.Error("SSO-enabled request with only a Bearer token should be refused")
	}
}

func TestHasBasicAuth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if hasBasicAuth(req) {
		t.Error("no Authorization header reported as Basic")
	}
	req.Header.Set("Authorization", "Basic cm9vdDpwYXNz")
	if !hasBasicAuth(req) {
		t.Error("Basic Authorization header not detected")
	}
	req.Header.Set("Authorization", "basic cm9vdDpwYXNz")
	if !hasBasicAuth(req) {
		t.Error("lowercase basic scheme should be detected (scheme is case-insensitive)")
	}
	req.Header.Set("Authorization", "Bearer abc")
	if hasBasicAuth(req) {
		t.Error("Bearer Authorization header reported as Basic")
	}
}
