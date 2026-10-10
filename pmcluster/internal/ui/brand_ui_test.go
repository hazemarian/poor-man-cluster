package ui

import (
	"net/http"
	"strings"
	"testing"
)

// TestBrandMarkInShellAndLogin asserts the mark, the two-part "Poor Man's Cluster" wordmark and
// the accent attribute render in both the operator shell and the standalone sign-in page, and that
// the retired brand.css / palette picker are gone.
func TestBrandMarkInShellAndLogin(t *testing.T) {
	daemon := fakeDaemon(t)
	defer daemon.Close()

	// Operator shell: login-disabled mode reaches /web/ without a session.
	cfg := FromEnv()
	cfg.DataDir = t.TempDir()
	cfg.PMAPIURL = daemon.URL
	cfg.PMAPIToken = "pmc_test"
	cfg.SessionSecret = []byte("0123456789abcdefgh")
	cfg.CookieName = "pmui_session"
	cfg.LoginDisabled = true
	shellApp, err := NewApp(cfg)
	if err != nil {
		t.Fatalf("NewApp(login disabled): %v", err)
	}
	resp := doRequest(t, shellApp, http.MethodGet, "/web/", "", map[string]*http.Cookie{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /web/ = %d, want 200", resp.StatusCode)
	}
	shell := readBody(t, resp)
	assertBrandMark(t, "shell", shell)
	for _, want := range []string{`class="brand-name"`, ">pmcluster</small>"} { // wordmark block + mono product id
		if !strings.Contains(shell, want) {
			t.Errorf("shell page missing %q", want)
		}
	}

	// Sign-in page: login enabled, public route.
	loginApp := newTestApp(t, daemon)
	resp = doRequest(t, loginApp, http.MethodGet, "/web/login", "", map[string]*http.Cookie{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /web/login = %d, want 200", resp.StatusCode)
	}
	assertBrandMark(t, "login", readBody(t, resp))
}

func assertBrandMark(t *testing.T, where, body string) {
	t.Helper()
	for _, want := range []string{
		`href="#brandmark"`,    // the shared inline mark symbol
		`id="brandmark"`,       // ...which ships in the icon sprite
		"Poor Man&#39;s",       // lead wordmark text (html/template escapes the apostrophe)
		"<b>Cluster</b>",       // the accent-colored second word
		`data-accent="orange"`, // the accent attribute, defaulting to the brand orange
		`href="/web/static/base.css"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s page missing %q", where, want)
		}
	}
	for _, gone := range []string{"brand.css", "data-palette", "pmc_palette"} {
		if strings.Contains(body, gone) {
			t.Errorf("%s page still carries the retired %q", where, gone)
		}
	}
}
