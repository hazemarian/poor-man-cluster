package edgeproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRealIP_TrustsForwardedForFromKnownProxy(t *testing.T) {
	var got string
	h := RealIP([]string{"10.0.0.0/8"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = realIP(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	req.RemoteAddr = "10.0.3.4:1" // Traefik, trusted
	req.Header.Set("X-Forwarded-For", "198.51.100.20, 10.0.3.4")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if got != "198.51.100.20" {
		t.Errorf("realIP = %q, want 198.51.100.20", got)
	}
}

func TestRealIP_StopsAtUntrustedHop(t *testing.T) {
	var got string
	h := RealIP([]string{"10.0.0.0/8"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = realIP(r.Context())
	}))

	// Traefik is trusted (RemoteAddr), but an attacker inserted a spoofed hop.
	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	req.RemoteAddr = "10.0.3.4:1"
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.20, 10.0.3.4")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	// Right-to-left: 10.0.3.4 trusted -> accept 198.51.100.20; then 198.51.100.20
	// is untrusted so we stop: the spoofed 6.6.6.6 must not be trusted.
	if got != "198.51.100.20" {
		t.Errorf("realIP = %q, want 198.51.100.20 (spoof ignored)", got)
	}
}

func TestRealIP_FallsBackToRemoteAddr(t *testing.T) {
	var got string
	h := RealIP([]string{"10.0.0.0/8"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = realIP(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	req.RemoteAddr = "198.51.100.9:1" // not a trusted proxy, no XFF
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if got != "198.51.100.9" {
		t.Errorf("realIP = %q, want 198.51.100.9", got)
	}
}
