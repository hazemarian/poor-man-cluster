package ui

import (
	"net/http"
	"strings"
	"testing"
)

// TestBrandLogoInShellAndLogin asserts the combination-mark lockup — the
// inlined isometric mark (palette-aware), the "Poor Man's Cluster" wordmark,
// the "pmcluster" mono subtext, the palette picker and the palette boot script
// — renders in both the operator shell and the standalone sign-in page.
func TestBrandLogoInShellAndLogin(t *testing.T) {
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
	assertBrandLockup(t, "shell", readBody(t, resp))

	// Sign-in page: login enabled, public route.
	loginApp := newTestApp(t, daemon)
	resp = doRequest(t, loginApp, http.MethodGet, "/web/login", "", map[string]*http.Cookie{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /web/login = %d, want 200", resp.StatusCode)
	}
	assertBrandLockup(t, "login", readBody(t, resp))
}

func assertBrandLockup(t *testing.T, where, body string) {
	t.Helper()
	for _, want := range []string{
		`class="logo-lockup"`,           // the combination-mark wrapper
		`class="logo-mark"`,             // the mark wrapper
		`viewBox="0 0 120 120"`,         // the mark is inlined, not an <img>
		`var(--brand-primary, #FF5722)`, // mark lines/LEDs follow the palette
		"Poor Man's",                    // primary wordmark text
		">Cluster</em>",                 // secondary wordmark text in the accent
		`class="logo-sub"`,              // mono subtext wrapper
		`href="/web/static/brand.css"`,  // the brand stylesheet is linked
		`data-palette-picker`,           // the palette picker is present
		`data-palette-choose="orange"`,  // all six palettes are offered
		`data-palette-choose="purple"`,
		`data-palette-choose="blue"`,
		`data-palette-choose="red"`,
		`data-palette-choose="green"`,
		`data-palette-choose="teal"`,
		"pmc_palette", // boot script + picker persistence key
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s page missing %q", where, want)
		}
	}
}
