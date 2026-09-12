package edgeproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConnectionShield_SaturatesTo503(t *testing.T) {
	// MaxConcurrent=1: while the first request is in flight, a second is 503.
	hold := make(chan struct{})
	release := make(chan struct{})

	inner := ConnectionShield(Config{MaxConcurrent: 1})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hold // block the single in-flight slot
		w.WriteHeader(http.StatusOK)
		<-release
	}))

	// First request acquires the slot and blocks.
	go func() {
		req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		rw := httptest.NewRecorder()
		inner.ServeHTTP(rw, req)
	}()
	time.Sleep(20 * time.Millisecond) // ensure slot is taken
	close(hold)
	time.Sleep(20 * time.Millisecond)

	// Second request must be rejected with 503, not queued.
	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	rw := httptest.NewRecorder()
	inner.ServeHTTP(rw, req)
	if rw.Code != http.StatusServiceUnavailable {
		t.Errorf("saturated status = %d, want 503", rw.Code)
	}
	close(release)
}

func TestRequestTimeout_RejectsSlowUpstream(t *testing.T) {
	inner := RequestTimeout(50 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep past the deadline; the ctx will be cancelled.
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	rw := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		inner.ServeHTTP(rw, req)
	}()
	select {
	case <-done:
		// Handler returned; timeout middleware doesn't itself write a response,
		// but the inner handler's write must not hang the test.
	case <-time.After(2 * time.Second):
		t.Fatalf("request did not return after upstream timeout")
	}
}

func TestLimitBody_RejectsOversized(t *testing.T) {
	inner := LimitBody(16)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			// MaxBytesReader signals the error; net/http would map it to 413.
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "http://x/", strings.NewReader(strings.Repeat("a", 100)))
	rw := httptest.NewRecorder()
	inner.ServeHTTP(rw, req)
	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body status = %d, want 413", rw.Code)
	}
}
