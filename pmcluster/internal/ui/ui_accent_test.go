package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// getWithCookies issues a GET carrying the session cookie plus any extra
// cookies. doRequest only forwards the session cookie, and the accent lives in
// its own pmc_accent cookie.
func getWithCookies(t *testing.T, app *App, path string, jar map[string]*http.Cookie, extra ...*http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://x"+path, nil)
	if ck, ok := jar[app.Auth.CookieName()]; ok {
		req.AddCookie(ck)
	}
	for _, c := range extra {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, req)
	return readBody(t, rr.Result())
}

func TestAccent_CookieDrivesDocumentAndSettingsPicker(t *testing.T) {
	daemon := fakeDaemon(t)
	defer daemon.Close()
	app := newTestApp(t, daemon)
	jar := map[string]*http.Cookie{}

	doRequest(t, app, http.MethodPost, "/web/setup", "username=admin&password=supersecret&confirm=supersecret", jar)
	doRequest(t, app, http.MethodPost, "/web/login", "username=admin&password=supersecret", jar)

	// No cookie: the brand orange, on the console and on the sign-in page.
	if body := getWithCookies(t, app, "/web/settings", jar); !strings.Contains(body, `data-accent="orange"`) {
		t.Errorf("settings without a pmc_accent cookie should default to orange")
	}

	// A valid cookie reaches <html>, checks its own radio and nothing else.
	violet := &http.Cookie{Name: "pmc_accent", Value: "violet"}
	body := getWithCookies(t, app, "/web/settings", jar, violet)
	if !strings.Contains(body, `data-accent="violet"`) {
		t.Errorf("settings should render data-accent=violet from the cookie")
	}
	if !strings.Contains(body, `value="violet" checked`) || strings.Contains(body, `value="orange" checked`) {
		t.Errorf("only the violet radio should be checked")
	}
	for _, a := range []string{"orange", "blue", "violet", "rose", "cyan"} {
		if !strings.Contains(body, `name="pmc-accent" value="`+a+`"`) {
			t.Errorf("accent picker is missing the %s option", a)
		}
	}
	if !strings.Contains(body, `id="brandmark"`) {
		t.Errorf("the brand mark sprite should ship with every page")
	}

	// A value outside the whitelist must never reach the attribute.
	evil := &http.Cookie{Name: "pmc_accent", Value: `red" onload="x`}
	if body := getWithCookies(t, app, "/web/settings", jar, evil); !strings.Contains(body, `data-accent="orange"`) || strings.Contains(body, `onload="x`) {
		t.Errorf("an unknown pmc_accent value must fall back to orange")
	}

	// The sign-in page honours the accent too (no session needed).
	if body := getWithCookies(t, app, "/web/login", map[string]*http.Cookie{}, &http.Cookie{Name: "pmc_accent", Value: "rose"}); !strings.Contains(body, `data-accent="rose"`) {
		t.Errorf("login page should render data-accent=rose from the cookie")
	}
}
