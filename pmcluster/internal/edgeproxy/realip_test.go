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
	req.RemoteAddr = "10.0.3.4:1"
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

	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	req.RemoteAddr = "10.0.3.4:1"
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.20, 10.0.3.4")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

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
	req.RemoteAddr = "198.51.100.9:1"
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if got != "198.51.100.9" {
		t.Errorf("realIP = %q, want 198.51.100.9", got)
	}
}
