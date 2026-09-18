package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
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

	mux.HandleFunc("/api/stacks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			write(w, `{"stacks":[{"name":"demo","current_revision":3,"repo_url":"https://example.com/demo","created_at":1,"updated_at":2}]}`)
		default:
			write(w, `{"stack":"demo","revision":3}`)
		}
	})

	mux.HandleFunc("/api/stacks/demo", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		write(w, `{"stack":{"name":"demo","current_revision":3,"repo_url":"https://example.com/demo"},"revisions":[{"revision":3,"created_at":30},{"revision":2,"created_at":20}],"last_backup":{"status":"succeeded","started_at":25}}`)
	})
	mux.HandleFunc("/api/stacks/demo/revisions/3", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"stack":"demo","revision":3,"created_at":30,"source_yaml":"app: demo\nversion: v3\n","rendered_yaml":"services:\n  demo:\n","payload":"{}"}`)
	})

	mux.HandleFunc("/api/stacks/demo/rollback", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"stack":"demo","new_revision":3,"rolled_back_to":2}`)
	})

	mux.HandleFunc("/api/stacks/demo/sync", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"stack":"demo","revision":4,"changed":true}`)
	})

	mux.HandleFunc("/api/stacks/demo/backups", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"backups":[{"id":9,"status":"succeeded","stack_name":"demo","revision":3,"archive_paths":["a.tar.gz"],"started_at":10,"finished_at":11}]}`)
	})

	mux.HandleFunc("/api/backups", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			write(w, `{"id":10,"status":"running","stack_name":"","revision":0,"archive_paths":[],"started_at":50,"finished_at":0}`)
		default:
			write(w, `{"backups":[{"id":9,"status":"succeeded","stack_name":"demo","revision":3,"archive_paths":["a.tar.gz"],"started_at":10,"finished_at":11}]}`)
		}
	})

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

	mux.HandleFunc("/api/secrets", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			write(w, `{"id":7,"scope":"service","name":"db_pass","hash":"a1b2c3"}`)
		default:
			write(w, `{"secrets":[{"id":6,"scope":"cluster","name":"site_cert","hash":"abc123","created_at":60}]}`)
		}
	})
	mux.HandleFunc("/api/secrets/db_pass", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			write(w, `{"name":"db_pass","hash":"z9"}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/secrets/db_pass/value", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"name":"db_pass","value":"the-decrypted-value"}`)
	})

	mux.HandleFunc("/api/configs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			write(w, `{"id":3,"scope":"service","name":"nginx_conf","kind":"file","version":"v0.2.30","hash":"h1","created_at":70,"updated_at":70}`)
		default:
			write(w, `{"configs":[{"id":2,"scope":"service","name":"app_env","kind":"env","version":"v0.2.30","hash":"h2","created_at":65,"updated_at":66},{"id":9,"scope":"cluster","name":"traefik-dynamic","kind":"template","version":"v0.2.40","hash":"h9","rendered":true,"created_at":65,"updated_at":66}]}`)
		}
	})
	mux.HandleFunc("/api/configs/nginx_conf", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			write(w, `{"name":"nginx_conf","hash":"h1-new"}`)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			write(w, `{"id":3,"scope":"service","name":"nginx_conf","kind":"file","version":"v0.2.30","hash":"h1","created_at":70,"updated_at":70,"content":"worker_processes 4;"}`)
		}
	})
	mux.HandleFunc("/api/configs/nginx_conf/versions", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"versions":[{"id":2,"hash":"h0","created_at":68},{"id":1,"hash":"h-1","created_at":60}]}`)
	})
	mux.HandleFunc("/api/configs/nginx_conf/rollback", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"name":"nginx_conf","hash":"h0","rolled_back_to":2}`)
	})
	mux.HandleFunc("/api/update", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"otel_config":"pmcluster_otel_config_v034","traefik_config":"pmcluster_traefik_dynamic_v044","cert_secret":"cert_v041","key_secret":"key_v041","edge_config":"pmcluster_edge_v005","stacks_deployed":["infra"]}`)
	})
	mux.HandleFunc("/api/cluster/rendered", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"configs":[{"name":"traefik-dynamic","content":"tls:\n  certificates: []\n"},{"name":"infra-stack","content":"version: \"3.9\"\nservices:\n  traefik:\n    image: traefik:v3.6.5\n"}]}`)
	})

	siteSoon := time.Now().AddDate(0, 0, 10).UTC().Format(time.RFC3339)
	siteFar := time.Now().AddDate(1, 0, 0).UTC().Format(time.RFC3339)
	mux.HandleFunc("/api/tls/site", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			write(w, `{"domain":"nextrum-sy.com","cert_secret":"cert_v041","key_secret":"key_v041","not_before":"2026-09-01T00:00:00Z","not_after":"`+siteFar+`","sans":["nextrum-sy.com","*.nextrum-sy.com"],"cert_hash":"cafe1234","key_hash":"beef5678","created_at":"2026-09-12T00:00:00Z","updated_at":"2026-09-16T00:00:00Z"}`)
		default:
			write(w, `{"domain":"nextrum-sy.com","cert_secret":"cert_v041","key_secret":"key_v041","not_before":"2026-09-01T00:00:00Z","not_after":"`+siteSoon+`","sans":["nextrum-sy.com"],"cert_hash":"cafe1234","key_hash":"beef5678","created_at":"2026-09-12T00:00:00Z","updated_at":"2026-09-16T00:00:00Z"}`)
		}
	})
	mux.HandleFunc("/api/tls/hosts", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"hosts":[{"host":"idlebbookfair.com","sans":["idlebbookfair.com","www.idlebbookfair.com"],"not_after":"`+siteFar+`","not_before":"2026-01-01T00:00:00Z","cert_secret":"hostcert-idlebbookfair-com_v001","key_secret":"hostkey-idlebbookfair-com_v001","cert_hash":"aa11","key_hash":"bb22","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`)
	})
	mux.HandleFunc("/api/tls/hosts/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			write(w, `{"host":"idlebbookfair.com","sans":["idlebbookfair.com"],"not_after":"`+siteFar+`","not_before":"2026-01-01T00:00:00Z","cert_secret":"hostcert-idlebbookfair-com_v001","key_secret":"hostkey-idlebbookfair-com_v001","cert_hash":"aa11","key_hash":"bb22","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})

	// Service ops (the Portainer replacement).
	mux.HandleFunc("/api/services", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"services":[{"name":"demo_web","stack":"demo","replicas":2,"desired":2,"image":"ghcr.io/nextrum-sy/demo:1.0","mode":"replicated","updated":70},{"name":"infra_traefik","stack":"infra","replicas":1,"desired":1,"image":"traefik:v3","mode":"global","updated":71}]}`)
	})
	mux.HandleFunc("/api/services/demo/web/tasks", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"service":"demo_web","tasks":[{"task_id":"t1","node":"mgr","slot":1,"state":"running","error":"","started_at":65,"finished_at":0}]}`)
	})
	mux.HandleFunc("/api/services/demo/web/logs", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"service":"demo_web","logs":[{"stream":"stdout","line":"listening on :8080"}]}`)
	})
	mux.HandleFunc("/api/services/demo/web/restart", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"service":"demo_web","restarted":true}`)
	})
	mux.HandleFunc("/api/services/demo/web/exec", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"service":"demo_web","exit_code":0,"stdout":"root","stderr":""}`)
	})
	return httptest.NewServer(mux)
}

func newTestApp(t *testing.T, daemon *httptest.Server) *App {
	t.Helper()
	cfg := FromEnv()
	cfg.DataDir = t.TempDir()
	cfg.PMAPIURL = daemon.URL
	cfg.PMAPIToken = "pmc_test"
	cfg.SessionSecret = []byte("0123456789abcdefgh")
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

	resp := doRequest(t, app, http.MethodGet, "/web/setup", "", jar)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /setup = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Set an admin password") {
		t.Errorf("setup page missing prompt, got: %s", body)
	}

	resp = doRequest(t, app, http.MethodPost, "/web/setup",
		"password=supersecret&confirm=supersecret", jar)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /setup = %d, want 200", resp.StatusCode)
	}

	resp = doRequest(t, app, http.MethodPost, "/web/login", "username=admin&password=wrong", jar)
	if resp.StatusCode != http.StatusOK || !strings.Contains(readBody(t, resp), "Invalid username") {
		t.Errorf("bad login should show an error")
	}

	resp = doRequest(t, app, http.MethodPost, "/web/login", "username=admin&password=supersecret", jar)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("POST /login = %d, want 302", resp.StatusCode)
	}
	cookied := len(jar[app.Auth.CookieName()].Value) > 0
	if !cookied {
		t.Fatalf("no session cookie issued after login")
	}

	delete(jar, app.Auth.CookieName())
	resp = doRequest(t, app, http.MethodGet, "/web/overview", "", jar)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/web/login" {
		t.Errorf("unauth /overview should redirect to /login")
	}
	doRequest(t, app, http.MethodGet, "/web/overview", "", jar)
	doRequest(t, app, http.MethodPost, "/web/login", "username=admin&password=supersecret", jar)

	resp = doRequest(t, app, http.MethodGet, "/web/overview", "", jar)
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
	doRequest(t, app, http.MethodPost, "/web/setup", "password=supersecret&confirm=supersecret", jar)
	doRequest(t, app, http.MethodPost, "/web/login", "username=admin&password=supersecret", jar)

	form := url.Values{"app_name": {"demo"}, "manifest": {"app: demo\nversion: v1\n"}}
	resp := doRequest(t, app, http.MethodPost, "/web/deploy", form.Encode(), jar)
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
	doRequest(t, app, http.MethodPost, "/web/setup", "password=supersecret&confirm=supersecret", jar)
	doRequest(t, app, http.MethodPost, "/web/login", "username=admin&password=supersecret", jar)

	resp := doRequest(t, app, http.MethodPost, "/web/settings",
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
		doRequest(t, app, http.MethodPost, "/web/setup", "password=supersecret&confirm=supersecret", jar)
		doRequest(t, app, http.MethodPost, "/web/login", "username=admin&password=supersecret", jar)
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

	login()

	assertFragment(http.MethodGet, "/web/", "", "operator console", "Overview", "Log out")
	assertFragment(http.MethodGet, "/web/overview", "", "manager-1", "Cluster overview")

	assertFragment(http.MethodGet, "/web/stacks", "", "Stacks", "demo", "3")
	assertFragment(http.MethodGet, "/web/stacks/demo", "", "Stack · demo", "Last backup:", "succeeded")
	assertFragment(http.MethodGet, "/web/stacks/demo/revisions/3", "", "Revision 3 · demo", "Source manifest")
	assertFragment(http.MethodGet, "/web/stacks/demo/backups", "", "Backups", "succeeded", "1 recent backup")

	assertFragment(http.MethodPost, "/web/stacks/demo/rollback", "revision=2", "Stack · demo", "Rolled back demo to revision 2")
	assertFragment(http.MethodPost, "/web/stacks/demo/sync", "", "Stack · demo", "Synced demo — revision 4")
	assertFragment(http.MethodPost, "/web/stacks/demo/remove", "", "Stacks", "Stack demo removed.")

	assertFragment(http.MethodPost, "/web/backups", "", "Backups", "Backup triggered.")

	assertFragment(http.MethodGet, "/web/deploy", "", "Deploy")
	assertFragment(http.MethodGet, "/web/settings", "", "Settings")

	resp := doRequest(t, app, http.MethodPost, "/web/logout", "", jar)
	if resp.StatusCode != http.StatusFound {
		t.Errorf("POST /logout = %d, want 302", resp.StatusCode)
	}
	if len(jar[app.Auth.CookieName()].Value) != 0 {
		t.Error("logout did not clear the session cookie")
	}

	delete(jar, app.Auth.CookieName())
	resp = doRequest(t, app, http.MethodGet, "/web/stacks", "", jar)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/web/login" {
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

	doRequest(t, app, http.MethodPost, "/web/setup", "password=supersecret&confirm=supersecret", jar)
	doRequest(t, app, http.MethodPost, "/web/login", "username=admin&password=supersecret", jar)

	resp := doRequest(t, app, http.MethodGet, "/web/webhooks", "", jar)
	b := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /webhooks = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"Webhook Keys", "github", "bookfair ci"} {
		if !strings.Contains(b, want) {
			t.Errorf("webhooks page missing %q; got: %s", want, b)
		}
	}

	resp = doRequest(t, app, http.MethodPost, "/web/webhooks", "source=github&description=bookfair ci", jar)
	b = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /webhooks = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"shown once", "the-one-time-secret-value", "Webhook source github created"} {
		if !strings.Contains(b, want) {
			t.Errorf("webhook create missing %q; got: %s", want, b)
		}
	}

	resp = doRequest(t, app, http.MethodPost, "/web/webhooks/remove/github", "", jar)
	b = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /webhooks/remove/github = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(b, "Removed webhook source github") {
		t.Errorf("webhook remove missing confirmation; got: %s", b)
	}

	resp = doRequest(t, app, http.MethodGet, "/web/apikeys", "", jar)
	b = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /apikeys = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(b, "admin") {
		t.Errorf("apikeys page missing admin; got: %s", b)
	}

	resp = doRequest(t, app, http.MethodPost, "/web/apikeys", "name=ci", jar)
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

	resp = doRequest(t, app, http.MethodPost, "/web/webhooks", "source=github&description=bookfair ci", jar)
	b = readBody(t, resp)
	if !strings.Contains(b, "Copy secret") {
		t.Errorf("webhook create missing copy button; got: %s", b)
	}

	resp = doRequest(t, app, http.MethodPost, "/web/apikeys/remove/2", "", jar)
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

// TestSecretsAndConfigs drives the Settings cluster-scope sections and the
// per-stack config page: list, create, edit, rollback, delete, secret reveal
// (with confirmation) and the apply-to-swarm update — exercising every new
// pmapi method (ListSecrets/CreateSecret/UpdateSecret/DeleteSecret/
// RevealSecret, ListConfigs/GetConfig/CreateConfig/UpdateConfig/
// ConfigVersions/RollbackConfig/DeleteConfig, TriggerUpdate).
func TestSecretsAndConfigs(t *testing.T) {
	daemon := fakeDaemon(t)
	defer daemon.Close()
	app := newTestApp(t, daemon)
	jar := map[string]*http.Cookie{}

	doRequest(t, app, http.MethodPost, "/web/setup", "password=supersecret&confirm=supersecret", jar)
	doRequest(t, app, http.MethodPost, "/web/login", "username=admin&password=supersecret", jar)

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

	// Settings surfaces the cluster-scope configs + secrets + apply button.
	// Secret values are masked; hashes are never listed.
	b := assertFragment(http.MethodGet, "/web/settings", "",
		"Settings", "Cluster configs", "Cluster secrets", "site_cert",
		"••••••••", "Apply to swarm", "Add Config", "Add Secret",
		"traefik-dynamic", `hx-get="/web/settings/rendered/traefik-dynamic"`)
	if strings.Contains(b, "topsecret") || strings.Contains(b, "abc123") {
		t.Errorf("secret value or hash leaked into the rendered page")
	}

	// Rendered configs view: the post-substitution YAML shown read-only.
	assertFragment(http.MethodGet, "/web/settings/rendered/traefik-dynamic", "",
		"Rendered config", "traefik-dynamic", "tls:", "certificates: []")

	// Cluster config lifecycle — create/edit/rollback via modals.
	assertFragment(http.MethodPost, "/web/settings/configs/add",
		"name=nginx_conf&kind=file&content=worker_processes 4;",
		"Config nginx_conf created")

	assertFragment(http.MethodGet, "/web/settings/configs/edit/nginx_conf", "",
		"Edit config", "worker_processes 4;", "Version history", "h0",
		"/web/settings/configs/rollback/nginx_conf/2")

	assertFragment(http.MethodPost, "/web/settings/configs/edit",
		"name=nginx_conf&content=worker_processes 8;", "Config nginx_conf updated")

	assertFragment(http.MethodPost, "/web/settings/configs/rollback/nginx_conf/2", "",
		"Config nginx_conf rolled back to version 2")

	// Cluster secret lifecycle — value never leaks; reveal requires confirm.
	b = assertFragment(http.MethodPost, "/web/settings/secrets/add",
		"name=db_pass&value=topsecret", "Secret db_pass created")
	if strings.Contains(b, "topsecret") {
		t.Errorf("secret value leaked back into the rendered page")
	}

	assertFragment(http.MethodGet, "/web/settings/secrets/edit/db_pass", "",
		"Edit secret", "Editing <code>db_pass</code>")

	assertFragment(http.MethodPost, "/web/settings/secrets/edit",
		"name=db_pass&value=newvalue", "Secret db_pass updated")

	assertFragment(http.MethodGet, "/web/settings/secrets/reveal/db_pass", "",
		"Secret ·", "the-decrypted-value", "Copy value")

	assertFragment(http.MethodGet, "/web/settings", "",
		`onclick="return confirm('Reveal secret site_cert`)

	assertFragment(http.MethodPost, "/web/settings/secrets/remove/db_pass", "", "Deleted secret db_pass.")

	// Apply to swarm triggers a cluster update via the daemon.
	assertFragment(http.MethodPost, "/web/settings/apply", "",
		"Cluster update applied", "infra")

	// Per-stack config & secrets page.
	assertFragment(http.MethodGet, "/web/stacks/demo/config", "",
		"Config &amp; secrets", "demo", "app_env")

	assertFragment(http.MethodPost, "/web/stacks/demo/configs/add",
		"name=demo_nginx_conf&kind=file&content=worker_processes 4;",
		"Config demo_nginx_conf created for stack demo")

	assertFragment(http.MethodGet, "/web/stacks/demo/configs/edit/nginx_conf", "",
		"Edit config", "worker_processes 4;", "h0",
		"/web/stacks/demo/configs/rollback/nginx_conf/2")

	assertFragment(http.MethodPost, "/web/stacks/demo/configs/edit",
		"name=nginx_conf&content=worker_processes 8;", "Config nginx_conf updated")

	assertFragment(http.MethodPost, "/web/stacks/demo/configs/remove/nginx_conf", "",
		"Deleted config nginx_conf.")

	b = assertFragment(http.MethodPost, "/web/stacks/demo/secrets/add",
		"name=demo_db&value=topsecret", "Secret demo_db created for stack demo")
	if strings.Contains(b, "topsecret") {
		t.Errorf("secret value leaked into the rendered page")
	}

	assertFragment(http.MethodGet, "/web/stacks/demo/secrets/edit/db_pass", "",
		"Edit secret", "Editing <code>db_pass</code>")

	assertFragment(http.MethodGet, "/web/stacks/demo/secrets/reveal/db_pass", "",
		"the-decrypted-value")

	assertFragment(http.MethodGet, "/web/stacks/demo/secrets/new", "",
		"Add secret", "/web/stacks/demo/secrets/add")

	assertFragment(http.MethodGet, "/web/stacks/demo/config", "",
		`onclick="return confirm('Reveal secret`)

	assertFragment(http.MethodPost, "/web/stacks/demo/secrets/remove/db_pass", "",
		"Deleted secret db_pass.")
}

func TestTLSMainAndHosts(t *testing.T) {
	daemon := fakeDaemon(t)
	defer daemon.Close()
	app := newTestApp(t, daemon)
	jar := map[string]*http.Cookie{}

	doRequest(t, app, http.MethodPost, "/web/setup", "password=supersecret&confirm=supersecret", jar)
	doRequest(t, app, http.MethodPost, "/web/login", "username=admin&password=supersecret", jar)

	resp := doRequest(t, app, http.MethodGet, "/web/tls", "", jar)
	b := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /tls = %d, want 200; body: %s", resp.StatusCode, b)
	}
	for _, want := range []string{
		"Main certificate", "nextrum-sy.com", "expires soon",
		"Per-host certificates", "idlebbookfair.com",
		"Upload / renew", "Add certificate",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("tls page missing %q; got: %s", want, b)
		}
	}

	resp = doRequest(t, app, http.MethodGet, "/web/tls/site/new", "", jar)
	b = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /tls/site/new = %d, want 200; body: %s", resp.StatusCode, b)
	}
	for _, want := range []string{"Upload / renew main certificate", `hx-post="/web/tls/site"`, `name="cert"`, `name="key"`} {
		if !strings.Contains(b, want) {
			t.Errorf("tls site form missing %q; got: %s", want, b)
		}
	}

	resp = doRequest(t, app, http.MethodGet, "/web/tls/hosts/new", "", jar)
	b = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /tls/hosts/new = %d, want 200; body: %s", resp.StatusCode, b)
	}
	for _, want := range []string{"Add a per-host certificate", `hx-post="/web/tls"`, `name="host"`} {
		if !strings.Contains(b, want) {
			t.Errorf("tls host form missing %q; got: %s", want, b)
		}
	}

	resp = doRequest(t, app, http.MethodPost, "/web/tls/site",
		"cert=-----BEGIN CERTIFICATE-----&key=-----BEGIN PRIVATE KEY-----", jar)
	b = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /tls/site = %d, want 200; body: %s", resp.StatusCode, b)
	}
	if !strings.Contains(b, "Site certificate for nextrum-sy.com updated") {
		t.Errorf("tls site upload missing confirmation; got: %s", b)
	}

	resp = doRequest(t, app, http.MethodPost, "/web/tls/site",
		"cert=-----BEGIN CERTIFICATE-----", jar)
	b = readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("POST /tls/site missing key = %d, want 422; body: %s", resp.StatusCode, b)
	}
	if !strings.Contains(b, "Certificate and private key are both required.") {
		t.Errorf("tls site missing-field error absent; got: %s", b)
	}

	resp = doRequest(t, app, http.MethodPost, "/web/tls",
		"host=idlebbookfair.com&cert=-----BEGIN CERTIFICATE-----&key=-----BEGIN PRIVATE KEY-----", jar)
	b = readBody(t, resp)
	if !strings.Contains(b, "Certificate for idlebbookfair.com stored") {
		t.Errorf("per-host add missing confirmation; got: %s", b)
	}
}

func TestServicesUI(t *testing.T) {
	daemon := fakeDaemon(t)
	defer daemon.Close()

	app := newTestApp(t, daemon)
	jar := map[string]*http.Cookie{}

	// login
	doRequest(t, app, http.MethodPost, "/web/setup", "password=supersecret&confirm=supersecret", jar)
	resp := doRequest(t, app, http.MethodPost, "/web/login", "username=admin&password=supersecret", jar)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login = %d, want 302", resp.StatusCode)
	}

	// services list
	resp = doRequest(t, app, http.MethodGet, "/web/services", "", jar)
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "demo_web") || !strings.Contains(s, "infra_traefik") {
		t.Errorf("services list missing rows; got: %s", s)
	}
	if !strings.Contains(s, "demo") || !strings.Contains(s, "2/2") {
		t.Errorf("services list missing stack/replica info; got: %s", s)
	}

	// tasks fragment for demo_web (unqualified: stack=demo, service=web)
	resp = doRequest(t, app, http.MethodGet, "/web/services/demo/web/tasks", "", jar)
	body, _ = io.ReadAll(resp.Body)
	s = string(body)
	if !strings.Contains(s, "t1") || !strings.Contains(s, "running") {
		t.Errorf("tasks fragment missing task row; got: %s", s)
	}
	if !strings.Contains(s, "Restart") {
		t.Errorf("tasks fragment missing restart button; got: %s", s)
	}

	// logs fragment
	resp = doRequest(t, app, http.MethodGet, "/web/services/demo/web/logs", "", jar)
	body, _ = io.ReadAll(resp.Body)
	s = string(body)
	if !strings.Contains(s, "listening on :8080") {
		t.Errorf("logs fragment missing output; got: %s", s)
	}

	// exec
	resp = doRequest(t, app, http.MethodPost, "/web/services/demo/web/exec", "argv=whoami", jar)
	body, _ = io.ReadAll(resp.Body)
	s = string(body)
	if !strings.Contains(s, "root") {
		t.Errorf("exec fragment missing output; got: %s", s)
	}
}

// TestFragmentRefreshRendersAppShell covers the deep-link refresh bug: a
// browser refresh on an HTMX fragment URL (e.g. /stacks) is a full page load
// (no HX-Request header) and must return the app shell with the fragment
// embedded in #view — styles and sidebar survive the refresh. HTMX partial
// loads (with HX-Request) must keep returning the bare fragment.
func TestFragmentRefreshRendersAppShell(t *testing.T) {
	daemon := fakeDaemon(t)
	defer daemon.Close()
	app := newTestApp(t, daemon)
	jar := map[string]*http.Cookie{}

	doRequest(t, app, http.MethodPost, "/web/setup", "password=supersecret&confirm=supersecret", jar)
	doRequest(t, app, http.MethodPost, "/web/login", "username=admin&password=supersecret", jar)

	fullPage := func(path string) string {
		t.Helper()
		resp := doRequest(t, app, http.MethodGet, path, "", jar)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
		}
		return readBody(t, resp)
	}
	hxPartial := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://x"+path, nil)
		req.Header.Set("HX-Request", "true")
		if ck, ok := jar[app.Auth.CookieName()]; ok {
			req.AddCookie(ck)
		}
		rr := httptest.NewRecorder()
		app.Handler().ServeHTTP(rr, req)
		for _, c := range rr.Result().Cookies() {
			jar[c.Name] = c
		}
		if rr.Code != http.StatusOK {
			t.Fatalf("HX GET %s = %d, want 200", path, rr.Code)
		}
		return rr.Body.String()
	}

	// Full page load of a fragment URL: app shell (style block + sidebar +
	// nav) wrapping the fragment content in #view.
	b := fullPage("/web/stacks")
	for _, want := range []string{
		"<style>", "operator console", "Overview", "Stacks", "Services",
		`id="view"`, "demo", "Log out",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("full-page GET /stacks missing %q", want)
		}
	}
	if strings.Contains(b, `<span hx-get="/web/overview" hx-trigger="load">`) {
		t.Errorf("full-page GET /stacks must not auto-load overview over the embedded fragment")
	}

	// HTMX partial load of the same URL: bare fragment, no shell.
	b = hxPartial("/web/stacks")
	for _, want := range []string{"demo", "Stacks"} {
		if !strings.Contains(b, want) {
			t.Errorf("HX GET /stacks missing %q", want)
		}
	}
	if strings.Contains(b, "<style>") || strings.Contains(b, "operator console") {
		t.Errorf("HX GET /stacks should return the bare fragment, got the shell")
	}
}
