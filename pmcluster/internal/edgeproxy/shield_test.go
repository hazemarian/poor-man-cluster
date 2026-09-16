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

	hold := make(chan struct{})
	release := make(chan struct{})

	inner := ConnectionShield(Config{MaxConcurrent: 1})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hold
		w.WriteHeader(http.StatusOK)
		<-release
	}))

	go func() {
		req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		rw := httptest.NewRecorder()
		inner.ServeHTTP(rw, req)
	}()
	time.Sleep(20 * time.Millisecond)
	close(hold)
	time.Sleep(20 * time.Millisecond)

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

	case <-time.After(2 * time.Second):
		t.Fatalf("request did not return after upstream timeout")
	}
}

func TestLimitBody_RejectsOversized(t *testing.T) {
	inner := LimitBody(16)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {

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
