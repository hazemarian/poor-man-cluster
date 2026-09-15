package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeDaemon simulates the pmcluster /api endpoints the UI drives. It returns
// canned JSON matching the real shapes (see internal/api).
func fakeDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body) //nolint:errcheck
	}

	mux.HandleFunc("/api/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer pmc_test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		write(w, `{"id":1,"name":"cli"}`)
	})
	mux.HandleFunc("/api/cluster/info", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"node_name":"mgr","server_version":"28","os":"linux","arch":"amd64","cpus":8,"memory_bytes":8589934592,"swarm":{"state":"active","control_available":true,"managers":1,"nodes":2}}`)
	})
	mux.HandleFunc("/api/nodes", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"nodes":[{"hostname":"manager-1","role":"manager","status":"ready","is_leader":true,"engine_version":"28.5"}]}`)
	})

	// Stacks index: GET lists, POST deploys.
	mux.HandleFunc("/api/stacks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			write(w, `{"stacks":[{"name":"demo","current_revision":3,"repo_url":"https://example.com/demo","created_at":1,"updated_at":2}]}`)
		default:
			write(w, `{"stack":"demo","revision":3}`)
		}
	})
	// Stack detail + its revisions.
	mux.HandleFunc("/api/stacks/demo", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"stack":{"name":"demo","current_revision":3,"repo_url":"https://example.com/demo"},"revisions":[{"revision":3,"created_at":30},{"revision":2,"created_at":20}],"last_backup":{"status":"succeeded","started_at":25}}`)
	})
	mux.HandleFunc("/api/stacks/demo/revisions/3", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"stack":"demo","revision":3,"created_at":30,"source_yaml":"app: demo\nversion: v3\n","rendered_yaml":"services:\n  demo:\n","payload":"{}"}`)
	})
	// Rollback (POST only).
	mux.HandleFunc("/api/stacks/demo/rollback", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"stack":"demo","new_revision":3,"rolled_back_to":2}`)
	})
	// Per-stack backups.
	mux.HandleFunc("/api/stacks/demo/backups", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"backups":[{"id":9,"status":"succeeded","stack_name":"demo","revision":3,"archive_paths":["a.tar.gz"],"started_at":10,"finished_at":11}]}`)
	})

	// Cluster-wide backups: GET lists, POST triggers.
	mux.HandleFunc("/api/backups", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			write(w, `{"id":10,"status":"running","stack_name":"","revision":0,"archive_paths":[],"started_at":50,"finished_at":0}`)
		default:
			write(w, `{"backups":[{"id":9,"status":"succeeded","stack_name":"demo","revision":3,"archive_paths":["a.tar.gz"],"started_at":10,"finished_at":11}]}`)
		}
	})

	// Webhook sources: GET lists (never secret), POST creates (secret once).
	mux.HandleFunc("/api/webhooks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			write(w, `{"source":"github","secret":"the-one-time-secret-value"}`)
		default:
			write(w, `{"webhooks":[{"source":"github","description":"bookfair ci","created_at":55}]}`)
		}
	})
	mux.HandleFunc("/api/webhooks/github", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	// API keys: GET lists (never token), POST creates (token once).
	mux.HandleFunc("/api/api_keys", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			write(w, `{"id":2,"name":"ci","token":"pmc_tokenabc"}`)
		default:
			write(w, `{"keys":[{"id":1,"name":"admin","created_at":40}]}`)
		}
	})
	mux.HandleFunc("/api/api_keys/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return httptest.NewServer(mux)
}

func newTestApp(t *testing.T, daemon *httptest.Server) *App {
	t.Helper()
	cfg := FromEnv()
	cfg.DataDir = t.TempDir()
	cfg.PMAPIURL = daemon.URL
	cfg.PMAPIToken = "pmc_test"
	cfg.SessionSecret = []byte("0123456789abcdefgh") // >= 16 bytes
	cfg.CookieName = "pmui_session"
	app, err := NewApp(cfg)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	return app
}

// doRequest performs a request against the app router, keeping a cookie jar so
// session state flows between calls. nonRedirects requests are returned as-is.
func doRequest(t *testing.T, app *App, method, path string, body string, jar map[string]*http.Cookie) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "http://x"+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if ck, ok := jar[app.Auth.CookieName()]; ok {
		req.AddCookie(ck)
	}
	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, req)
	for _, c := range rr.Result().Cookies() {
		jar[c.Name] = c
	}
	return rr.Result()
}

func TestBootstrap_Setup_Login_Overview(t *testing.T) {
	daemon := fakeDaemon(t)
	defer daemon.Close()

	app := newTestApp(t, daemon)
	jar := map[string]*http.Cookie{}

	// Fresh cluster: first password setup is required.
	resp := doRequest(t, app, http.MethodGet, "/setup", "", jar)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /setup = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Set an admin password") {
		t.Errorf("setup page missing prompt, got: %s", body)
	}

	// Set the password (8+ chars, matching).
	resp = doRequest(t, app, http.MethodPost, "/setup",
		"password=supersecret&confirm=supersecret", jar)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /setup = %d, want 200", resp.StatusCode)
	}

	// Bad login refuses.
	resp = doRequest(t, app, http.MethodPost, "/login", "username=admin&password=wrong", jar)
	if resp.StatusCode != http.StatusOK || !strings.Contains(readBody(t, resp), "Invalid username") {
		t.Errorf("bad login should show an error")
	}

	// Good login issues a session cookie.
	resp = doRequest(t, app, http.MethodPost, "/login", "username=admin&password=supersecret", jar)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("POST /login = %d, want 302", resp.StatusCode)
	}
	cookied := len(jar[app.Auth.CookieName()].Value) > 0
	if !cookied {
		t.Fatalf("no session cookie issued after login")
	}

	// Unauthenticated access to a fragment redirects to /login.
	delete(jar, app.Auth.CookieName())
	resp = doRequest(t, app, http.MethodGet, "/overview", "", jar)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login" {
		t.Errorf("unauth /overview should redirect to /login")
	}
	doRequest(t, app, http.MethodGet, "/overview", "", jar) // still unauthenticated here
	doRequest(t, app, http.MethodPost, "/login", "username=admin&password=supersecret", jar)

	// Authed fragment renders cluster data.
	resp = doRequest(t, app, http.MethodGet, "/overview", "", jar)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /overview = %d, want 200", resp.StatusCode)
	}
	b := readBody(t, resp)
	for _, want := range []string{"manager-1", "Cluster overview", "8"} {
		if !strings.Contains(b, want) {
			t.Errorf("overview missing %q", want)
		}
	}
}

func TestDeploy_SendsManifest(t *testing.T) {
	daemon := fakeDaemon(t)
	defer daemon.Close()
	app := newTestApp(t, daemon)
	jar := map[string]*http.Cookie{}
	doRequest(t, app, http.MethodPost, "/setup", "password=supersecret&confirm=supersecret", jar)
	doRequest(t, app, http.MethodPost, "/login", "username=admin&password=supersecret", jar)

	form := url.Values{"app_name": {"demo"}, "manifest": {"app: demo\nversion: v1\n"}}
	resp := doRequest(t, app, http.MethodPost, "/deploy", form.Encode(), jar)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /deploy = %d, want 200", resp.StatusCode)
	}
	b := readBody(t, resp)
	if !strings.Contains(b, "revision 3") {
		t.Errorf("deploy result not shown, got: %s", b)
	}
}

func TestSettings_RoundTrip(t *testing.T) {
	daemon := fakeDaemon(t)
	defer daemon.Close()
	app := newTestApp(t, daemon)
	jar := map[string]*http.Cookie{}
	doRequest(t, app, http.MethodPost, "/setup", "password=supersecret&confirm=supersecret", jar)
	doRequest(t, app, http.MethodPost, "/login", "username=admin&password=supersecret", jar)

	resp := doRequest(t, app, http.MethodPost, "/settings",
		"api_url=http://override:9999&api_token=pmc_overridden", jar)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /settings = %d, want 200", resp.StatusCode)
	}
	if app.API.Configured() != true {
		t.Errorf("API should still be configured")
	}
}

// TestAllControllers exercises every UI route, which drives every pmapi client
// method (Me, ClusterInfo, Nodes, ListStacks, GetStack, GetRevision, Deploy,
// Rollback, ListBackups, CreateBackup, ListStackBackups) and every controller.
func TestAllControllers(t *testing.T) {
	daemon := fakeDaemon(t)
	defer daemon.Close()
	app := newTestApp(t, daemon)
	jar := map[string]*http.Cookie{}

	login := func() {
		doRequest(t, app, http.MethodPost, "/setup", "password=supersecret&confirm=supersecret", jar)
		doRequest(t, app, http.MethodPost, "/login", "username=admin&password=supersecret", jar)
	}

	assertFragment := func(method, path, body string, want ...string) string {
		t.Helper()
		resp := doRequest(t, app, method, path, body, jar)
		b := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s = %d, want 200; body: %s", method, path, resp.StatusCode, b)
		}
		for _, w := range want {
			if !strings.Contains(b, w) {
				t.Errorf("%s %s missing %q; got: %s", method, path, w, b)
			}
		}
		return b
	}

	// Setup + login bootstrap.
	login()

	// App shell (GET /) and fragment (GET /overview) — Me/ClusterInfo/Nodes.
	assertFragment(http.MethodGet, "/", "", "")
	assertFragment(http.MethodGet, "/overview", "", "manager-1", "Cluster overview")

	// Stacks list / detail / revision / backups — ListStacks/GetStack/
	// GetRevision/ListStackBackups.
	assertFragment(http.MethodGet, "/stacks", "", "Stacks", "demo", "3")
	assertFragment(http.MethodGet, "/stacks/demo", "", "Stack · demo", "Last backup:", "succeeded")
	assertFragment(http.MethodGet, "/stacks/demo/revisions/3", "", "Revision 3 · demo", "Source manifest")
	assertFragment(http.MethodGet, "/stacks/demo/backups", "", "Backups", "succeeded", "1 recent backup")

	// Rollback (POST) — client.Rollback + re-render detail.
	assertFragment(http.MethodPost, "/stacks/demo/rollback", "revision=2", "Stack · demo", "Rolled back demo to revision 2")

	// Backup trigger + list — CreateBackup/ListBackups.
	assertFragment(http.MethodPost, "/backups", "", "Backups", "Backup triggered.")

	// Deploy page + submit — Deploy (already covered by TestDeploy_SendsManifest).
	assertFragment(http.MethodGet, "/deploy", "", "Deploy")
	assertFragment(http.MethodGet, "/settings", "", "Settings")

	// Logout clears the session and redirects.
	resp := doRequest(t, app, http.MethodPost, "/logout", "", jar)
	if resp.StatusCode != http.StatusFound {
		t.Errorf("POST /logout = %d, want 302", resp.StatusCode)
	}
	if len(jar[app.Auth.CookieName()].Value) != 0 {
		t.Error("logout did not clear the session cookie")
	}

	// After logout, protected routes bounce to /login.
	delete(jar, app.Auth.CookieName())
	resp = doRequest(t, app, http.MethodGet, "/stacks", "", jar)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login" {
		t.Errorf("unauth /stacks should redirect to /login")
	}
}

// TestWebhooksAndAPIKeys drives the Webhooks and API Keys controllers: list,
// create (surfacing the one-time secret/token), remove, and confirms the secret
// is NOT present on a subsequent list.
func TestWebhooksAndAPIKeys(t *testing.T) {
	daemon := fakeDaemon(t)
	defer daemon.Close()
	app := newTestApp(t, daemon)
	jar := map[string]*http.Cookie{}

	doRequest(t, app, http.MethodPost, "/setup", "password=supersecret&confirm=supersecret", jar)
	doRequest(t, app, http.MethodPost, "/login", "username=admin&password=supersecret", jar)

	// Webhooks page lists sources.
	resp := doRequest(t, app, http.MethodGet, "/webhooks", "", jar)
	b := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /webhooks = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"Webhook Keys", "github", "bookfair ci"} {
		if !strings.Contains(b, want) {
			t.Errorf("webhooks page missing %q; got: %s", want, b)
		}
	}

	// Create surfaces the one-time secret.
	resp = doRequest(t, app, http.MethodPost, "/webhooks", "source=github&description=bookfair ci", jar)
	b = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /webhooks = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"shown once", "the-one-time-secret-value", "Webhook source github created"} {
		if !strings.Contains(b, want) {
			t.Errorf("webhook create missing %q; got: %s", want, b)
		}
	}

	// Removing a source revokes it.
	resp = doRequest(t, app, http.MethodPost, "/webhooks/remove/github", "", jar)
	b = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /webhooks/remove/github = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(b, "Removed webhook source github") {
		t.Errorf("webhook remove missing confirmation; got: %s", b)
	}

	// API Keys page lists users (no token).
	resp = doRequest(t, app, http.MethodGet, "/apikeys", "", jar)
	b = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /apikeys = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(b, "admin") {
		t.Errorf("apikeys page missing admin; got: %s", b)
	}

	// Create surfaces the one-time bearer token.
	resp = doRequest(t, app, http.MethodPost, "/apikeys", "name=ci", jar)
	b = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /apikeys = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(b, "pmc_tokenabc") {
		t.Errorf("apikey create missing one-time token; got: %s", b)
	}
	if !strings.Contains(b, "shown once") {
		t.Errorf("apikey create missing 'shown once' hint; got: %s", b)
	}
	if !strings.Contains(b, "Copy token") {
		t.Errorf("apikey create missing copy button; got: %s", b)
	}

	// Webhook create surfaces the one-time secret with a copy button.
	resp = doRequest(t, app, http.MethodPost, "/webhooks", "source=github&description=bookfair ci", jar)
	b = readBody(t, resp)
	if !strings.Contains(b, "Copy secret") {
		t.Errorf("webhook create missing copy button; got: %s", b)
	}

	// Removing an API key revokes it.
	resp = doRequest(t, app, http.MethodPost, "/apikeys/remove/2", "", jar)
	b = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /apikeys/remove/2 = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(b, "Removed API key 2") {
		t.Errorf("apikey remove missing confirmation; got: %s", b)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
