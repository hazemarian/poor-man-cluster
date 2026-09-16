package edgeproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBlocklist_DeniedCIDR(t *testing.T) {
	cfg := Config{BannedCIDRs: []string{"198.51.100.0/24"}}
	sb := &sharedBan{store: newBanStore()}
	h := Blocklist(cfg, sb)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	req.RemoteAddr = "198.51.100.44:1"
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Errorf("blocked CIDR status = %d, want 403", rw.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "http://x/", nil)
	req.RemoteAddr = "203.0.113.9:1"
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Errorf("allowed status = %d, want 200", rw.Code)
	}
}

func TestBlocklist_AllowlistDeniesOutside(t *testing.T) {
	cfg := Config{AllowedCIDRs: []string{"203.0.113.0/24"}}
	sb := &sharedBan{store: newBanStore()}
	h := Blocklist(cfg, sb)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	req.RemoteAddr = "8.8.8.8:1"
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Errorf("outside allowlist = %d, want 403", rw.Code)
	}
}

// buildTrusted returns a handler wrapping inner with RealIP (trusting the
// manager overlay 10/8) so realIP(ctx) is populated for keying on client IP.
// Requests use RemoteAddr=10.0.3.4 with X-Forwarded-For:<client>, 10.0.3.4.
func TestAutoBan_BansAfterThreshold(t *testing.T) {
	cfg := Config{
		BanThreshold: 3,
		BanDuration:  time.Minute,
		TrustedCIDRs: []string{"10.0.0.0/8"},
	}
	sb := &sharedBan{store: newBanStore()}
	clientIP := "198.51.100.77"

	inner := AutoBan(cfg, sb)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	chain := RealIP(cfg.TrustedCIDRs)(inner)

	for i := 0; i < cfg.BanThreshold; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://x/api/stacks", nil)
		req.RemoteAddr = "10.0.3.4:1"
		req.Header.Set("X-Forwarded-For", clientIP+", 10.0.3.4")
		rw := httptest.NewRecorder()
		chain.ServeHTTP(rw, req)
		if rw.Code != http.StatusUnauthorized {
			t.Fatalf("pre-threshold request %d = %d, want 401", i, rw.Code)
		}
	}

	block := RealIP(cfg.TrustedCIDRs)(Blocklist(cfg, sb)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	req := httptest.NewRequest(http.MethodGet, "http://x/api/stacks", nil)
	req.RemoteAddr = "10.0.3.4:1"
	req.Header.Set("X-Forwarded-For", clientIP+", 10.0.3.4")
	rw := httptest.NewRecorder()
	block.ServeHTTP(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Errorf("post-threshold status = %d, want 403 (auto-banned)", rw.Code)
	}
}
