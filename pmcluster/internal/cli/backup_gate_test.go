package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The gate must try every control-plane address before giving up: a worker's own
// daemon stands by and answers nothing, so the first candidate legitimately
// fails on the nodes that host most of the stacks.
func TestFetchBackupDecisionFallsThroughToAWorkingManager(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/backup/decision" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"enabled":true,"due":true,"schedule":"0 * * * *","store":true,"offsite":false}`))
	}))
	defer good.Close()

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "standing by", http.StatusServiceUnavailable)
	}))
	defer dead.Close()

	dec, err := fetchBackupDecision([]string{dead.URL, good.URL}, "node-1")
	if err != nil {
		t.Fatalf("expected the second address to answer: %v", err)
	}
	if !dec.Enabled || !dec.Due || dec.Schedule != "0 * * * *" || !dec.Store {
		t.Fatalf("decoded %+v", dec)
	}
}

func TestFetchBackupDecisionReportsWhenNothingAnswers(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "standing by", http.StatusServiceUnavailable)
	}))
	defer dead.Close()

	if _, err := fetchBackupDecision([]string{dead.URL}, "n"); err == nil {
		t.Fatal("expected an error when no control-plane address answers")
	}
	if _, err := fetchBackupDecision(nil, "n"); err == nil {
		t.Fatal("expected an error with no addresses at all")
	}
}
