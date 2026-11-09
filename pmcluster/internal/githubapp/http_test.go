package githubapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestHTTP_StatusAndTest(t *testing.T) {
	l, fake, _ := newLocal(t)
	if err := l.Save(context.Background(), 4242, 7, testKeyPEM(t), "", ""); err != nil {
		t.Fatalf("Save: %v", err)
	}
	r := chi.NewRouter()
	(&HTTP{App: l}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/github/app")
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var st Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !st.Configured || st.AppID != 4242 || st.Permissions["contents"] != "read" {
		t.Fatalf("status = %+v", st)
	}

	// The Test button mints a token; with no probe target it still answers OK
	// with the hint rather than pretending it proved read access.
	post, err := srv.Client().Post(srv.URL+"/github/app/test", "application/json", http.NoBody)
	if err != nil {
		t.Fatalf("POST test: %v", err)
	}
	defer func() { _ = post.Body.Close() }()
	var res TestResult
	if err := json.NewDecoder(post.Body).Decode(&res); err != nil {
		t.Fatalf("decode test: %v", err)
	}
	if !res.OK || !strings.Contains(res.Message, "probe") {
		t.Errorf("test result = %+v, want OK with a probe hint", res)
	}
	if fake.tokenCalls.Load() != 1 {
		t.Errorf("minted %d tokens, want 1", fake.tokenCalls.Load())
	}
}

func TestHTTP_Unconfigured(t *testing.T) {
	l, _, _ := newLocal(t)
	r := chi.NewRouter()
	(&HTTP{App: l}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/github/app")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var st Status
	_ = json.NewDecoder(resp.Body).Decode(&st)
	if st.Configured {
		t.Error("an empty store reported a configured app")
	}
}
