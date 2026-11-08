package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAdoptionGateAnswersRetryableServiceUnavailable is the "new leader is not
// yet serving" contract: while the store is being swapped, a request that
// depends on control-plane state is answered 503 (retry) instead of being served
// from the revision being replaced, and liveness stays green.
func TestAdoptionGateAnswersRetryableServiceUnavailable(t *testing.T) {
	var served int
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusOK)
	})
	gate := &adoptionGate{}
	h := gate.wrap(next)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nodes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ungated request = %d, want 200", rec.Code)
	}

	end := gate.begin()
	defer end()

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nodes", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("request during adoption = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("a 503 during adoption must tell the caller when to retry (Retry-After)")
	}
	if served != 1 {
		t.Fatalf("a request during adoption must not reach the handler (served %d times)", served)
	}

	// Liveness is never gated: a leader that looks down mid-adoption would be
	// replaced by another one.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK || served != 2 {
		t.Fatalf("/health during adoption = %d (served %d times), want 200 and served", rec.Code, served)
	}

	end()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nodes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("request after adoption = %d, want 200", rec.Code)
	}
}

// TestAdoptionGateEndsTheWindowEvenOnError: the caller ends the window
// unconditionally, so a failed adoption cannot leave the daemon answering 503
// forever.
func TestAdoptionGateEndsTheWindowEvenOnError(t *testing.T) {
	gate := &adoptionGate{}
	func() {
		defer gate.begin()()
		if !gate.adopting() {
			t.Fatal("the window must be open while the adoption runs")
		}
	}()
	if gate.adopting() {
		t.Fatal("the window must close when the adoption returns, error or not")
	}
}
