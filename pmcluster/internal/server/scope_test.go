package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/apikeys"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/manifest"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/services"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/settings"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// ---------------------------------------------------------------------------
// (a) target resolution
// ---------------------------------------------------------------------------

// TestStackTarget pins down how the guard decides which stack a request
// operates on: the first path segment of /api/stacks{,…} and
// /api/services{,…}, the deploy payload for POST /api/stacks, and "not the
// stack surface" for every other route.
func TestStackTarget(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string

		wantTarget  string
		wantSurface bool
	}{
		{name: "stack show", method: http.MethodGet, path: "/api/stacks/demo", wantTarget: "demo", wantSurface: true},
		{name: "stack sync", method: http.MethodPost, path: "/api/stacks/demo/sync", wantTarget: "demo", wantSurface: true},
		{name: "stack rollback", method: http.MethodPost, path: "/api/stacks/demo/rollback", wantTarget: "demo", wantSurface: true},
		{name: "stack delete", method: http.MethodDelete, path: "/api/stacks/demo", wantTarget: "demo", wantSurface: true},
		{name: "stack revisions", method: http.MethodGet, path: "/api/stacks/demo/revisions/1700000000", wantTarget: "demo", wantSurface: true},
		{name: "stack backups", method: http.MethodGet, path: "/api/stacks/demo/backups", wantTarget: "demo", wantSurface: true},
		{name: "stack list", method: http.MethodGet, path: "/api/stacks", wantTarget: "", wantSurface: true},
		{name: "trailing slash", method: http.MethodGet, path: "/api/stacks/", wantTarget: "", wantSurface: true},
		{name: "services collection", method: http.MethodGet, path: "/api/services", wantTarget: "", wantSurface: true},
		{name: "services stack", method: http.MethodGet, path: "/api/services/demo", wantTarget: "demo", wantSurface: true},
		{name: "service tasks", method: http.MethodGet, path: "/api/services/demo/web/tasks", wantTarget: "demo", wantSurface: true},
		{name: "service logs", method: http.MethodGet, path: "/api/services/demo/web/logs", wantTarget: "demo", wantSurface: true},
		{name: "service restart", method: http.MethodPost, path: "/api/services/demo/web/restart", wantTarget: "demo", wantSurface: true},
		{name: "service exec", method: http.MethodPost, path: "/api/services/demo/web/exec", wantTarget: "demo", wantSurface: true},
		{
			name:   "deploy names its stack in app_name",
			method: http.MethodPost, path: "/api/stacks",
			body:        `{"app_name":"other","manifest":"app: demo"}`,
			wantTarget:  "other",
			wantSurface: true,
		},
		{
			name:   "deploy falls back to the manifest app name",
			method: http.MethodPost, path: "/api/stacks",
			body:        `{"manifest":"app: frommanifest"}`,
			wantTarget:  "frommanifest",
			wantSurface: true,
		},
		{
			name:   "deploy with an unparseable body resolves nothing",
			method: http.MethodPost, path: "/api/stacks",
			body:        `not json`,
			wantTarget:  "",
			wantSurface: true,
		},
		{name: "deploy without a body resolves nothing", method: http.MethodPost, path: "/api/stacks", wantTarget: "", wantSurface: true},
		{name: "prefix lookalike is not the stack surface", method: http.MethodGet, path: "/api/stacksx", wantTarget: "", wantSurface: false},
		{name: "settings are not the stack surface", method: http.MethodGet, path: "/api/cluster/settings", wantTarget: "", wantSurface: false},
		{name: "me is not the stack surface", method: http.MethodGet, path: "/api/me", wantTarget: "", wantSurface: false},
		{name: "api_keys are not the stack surface", method: http.MethodGet, path: "/api/api_keys", wantTarget: "", wantSurface: false},
		{name: "usage is not the stack surface", method: http.MethodGet, path: "/api/usage/refs", wantTarget: "", wantSurface: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			req := httptest.NewRequest(tc.method, tc.path, body)

			target, surface := stackTarget(req)
			if surface != tc.wantSurface {
				t.Errorf("surface = %v, want %v", surface, tc.wantSurface)
			}
			if target != tc.wantTarget {
				t.Errorf("target = %q, want %q", target, tc.wantTarget)
			}

			// The deploy peek must never consume the body: whatever the
			// guard read has to still be readable afterwards.
			if tc.method == http.MethodPost && tc.body != "" {
				got, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read body after peek: %v", err)
				}
				if string(got) != tc.body {
					t.Errorf("body after peek = %q, want %q", got, tc.body)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// (b) middleware behaviour
// ---------------------------------------------------------------------------

// scopeMux wires the real Bearer + stackScopeGuard chain around stub routes
// so the guard can be exercised on its own.
func scopeMux() http.Handler {
	ok := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}
	echoBody := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"body":`+string(b)+`}`)
	}
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Bearer(&fakeLookup{users: map[string]*auth.User{
			"scoped-token": {ID: 1, Name: "ci", Stack: "demo"},
			"open-token":   {ID: 2, Name: "admin"},
		}}))
		r.Use(stackScopeGuard)
		r.Get("/me", ok)
		r.Get("/cluster/settings", ok)
		r.Get("/stacks", ok)
		r.Post("/stacks", echoBody)
		r.Get("/stacks/{name}", ok)
		r.Post("/stacks/{name}/sync", ok)
		r.Get("/services", ok)
		r.Get("/services/{stack}", ok)
	})
	return r
}

// TestStackScopeGuard_UnscopedTokenPassesEverything asserts backward
// compatibility: a token without a stack scope reaches every route, stack
// and non-stack alike, exactly as before scoping existed.
func TestStackScopeGuard_UnscopedTokenPassesEverything(t *testing.T) {
	h := scopeMux()
	reqs := []struct{ method, path string }{
		{http.MethodGet, "/api/me"},
		{http.MethodGet, "/api/cluster/settings"},
		{http.MethodGet, "/api/stacks"},
		{http.MethodGet, "/api/stacks/demo"},
		{http.MethodGet, "/api/stacks/other"},
		{http.MethodPost, "/api/stacks/other/sync"},
		{http.MethodGet, "/api/services"},
		{http.MethodGet, "/api/services/other"},
	}
	for _, rq := range reqs {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(rq.method, rq.path, nil)
		req.Header.Set("Authorization", "Bearer open-token")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s = %d, want 200", rq.method, rq.path, rec.Code)
		}
	}

	// Deploy bodies still reach the handler untouched.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/stacks",
		strings.NewReader(`{"app_name":"other","manifest":"app: other"}`))
	req.Header.Set("Authorization", "Bearer open-token")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/stacks = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"manifest":"app: other"`) {
		t.Errorf("deploy body altered by the guard: %s", rec.Body.String())
	}
}

// TestStackScopeGuard_ScopedTokenIsConfinedToItsStack covers the enforcement
// contract: the token's own stack passes, every other stack and every
// non-stack route is refused with 403 + {"error":"token scoped to stack <x>"}.
func TestStackScopeGuard_ScopedTokenIsConfinedToItsStack(t *testing.T) {
	h := scopeMux()

	allowed := []struct{ method, path string }{
		{http.MethodGet, "/api/stacks/demo"},
		{http.MethodPost, "/api/stacks/demo/sync"},
		{http.MethodGet, "/api/services/demo"},
	}
	for _, rq := range allowed {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(rq.method, rq.path, nil)
		req.Header.Set("Authorization", "Bearer scoped-token")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s = %d, want 200", rq.method, rq.path, rec.Code)
		}
	}

	denied := []struct{ method, path string }{
		{http.MethodGet, "/api/stacks"},
		{http.MethodGet, "/api/stacks/other"},
		{http.MethodPost, "/api/stacks/other/sync"},
		{http.MethodGet, "/api/services"},
		{http.MethodGet, "/api/services/other"},
		{http.MethodGet, "/api/me"},
		{http.MethodGet, "/api/cluster/settings"},
	}
	for _, rq := range denied {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(rq.method, rq.path, nil)
		req.Header.Set("Authorization", "Bearer scoped-token")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", rq.method, rq.path, rec.Code)
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode 403 body: %v", err)
		}
		if body["error"] != "token scoped to stack demo" {
			t.Errorf("%s %s error = %v, want %q", rq.method, rq.path, body["error"], "token scoped to stack demo")
		}
	}

	t.Run("deploy is checked against the payload", func(t *testing.T) {
		deploys := []struct {
			body     string
			wantCode int
		}{
			{`{"app_name":"demo","manifest":"app: demo"}`, http.StatusOK},
			{`{"manifest":"app: demo"}`, http.StatusOK},
			{`{"app_name":"other","manifest":"app: demo"}`, http.StatusForbidden},
			{`{"manifest":"app: other"}`, http.StatusForbidden},
			{`not json at all`, http.StatusForbidden},
		}
		for _, d := range deploys {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/stacks", strings.NewReader(d.body))
			req.Header.Set("Authorization", "Bearer scoped-token")
			h.ServeHTTP(rec, req)
			if rec.Code != d.wantCode {
				t.Errorf("POST /api/stacks %s = %d, want %d", d.body, rec.Code, d.wantCode)
			}
		}
	})

	t.Run("no token still means 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stacks/demo", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated = %d, want 401 (the guard must not bypass Bearer)", rec.Code)
		}
	})
}

// ---------------------------------------------------------------------------
// (c) HTTP-level behaviour on the fully-wired server
// ---------------------------------------------------------------------------

// fakeDeployer is a no-op stacks.Deployer that still resolves the target
// stack the way Deploy does, so handler responses prove the guard handed the
// body through intact.
type fakeDeployer struct{}

func (fakeDeployer) Deploy(_ context.Context, p stacks.Payload) (*stacks.Result, error) {
	name := p.AppName
	if name == "" {
		app, err := manifest.Parse([]byte(p.Manifest))
		if err != nil {
			return nil, err
		}
		name = app.Name
	}
	return &stacks.Result{StackName: name, Revision: 1700000001}, nil
}

func (fakeDeployer) Sync(_ context.Context, stackName string) (*stacks.Result, error) {
	return &stacks.Result{StackName: stackName, Revision: 1700000002, Changed: true}, nil
}

func (fakeDeployer) Rollback(_ context.Context, stackName string, rev int64) (*stacks.Result, error) {
	return &stacks.Result{StackName: stackName, Revision: rev}, nil
}

func (fakeDeployer) Undeploy(_ context.Context, stackName string) error { return nil }

// scopeEnv is a fully-wired daemon whose bearer lookup goes through a real
// store, so tokens minted by the apikeys service authenticate for real.
type scopeEnv struct {
	srv      *httptest.Server
	store    *store.Store
	scoped   string // token scoped to the "demo" stack
	unscoped string
}

func newScopeEnv(t *testing.T) *scopeEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seedStack(t, st, "demo")
	seedStack(t, st, "other")

	keys := apikeys.NewLocal(st)
	_, scoped, err := keys.Create(context.Background(), "ci", "demo")
	if err != nil {
		t.Fatalf("Create (scoped): %v", err)
	}
	_, unscoped, err := keys.Create(context.Background(), "admin")
	if err != nil {
		t.Fatalf("Create (unscoped): %v", err)
	}

	srv := httptest.NewServer(New(Deps{
		Lookup:        st,
		Store:         st,
		Settings:      settings.NewLocal(st),
		DeployService: fakeDeployer{},
		APIKeys:       keys,
		Services: &fakeServices{
			list: []services.ServiceSummary{
				{Name: "demo_web", Stack: "demo", Replicas: 1, Desired: 1, Mode: "replicated"},
				{Name: "other_web", Stack: "other", Replicas: 1, Desired: 1, Mode: "replicated"},
			},
			tasks:   []services.TaskRun{{TaskID: "t1", State: "running"}},
			logs:    []services.LogLine{{Stream: "stdout", Line: "hi"}},
			execRes: &services.ExecResult{ExitCode: 0, Stdout: "root"},
		},
	}))
	t.Cleanup(srv.Close)

	return &scopeEnv{srv: srv, store: st, scoped: scoped, unscoped: unscoped}
}

// seedStack records one revision so stack routes resolve to a real 200.
func seedStack(t *testing.T, st *store.Store, name string) {
	t.Helper()
	rev := &store.StackRevision{
		StackName:    name,
		Revision:     1700000000,
		SourceYAML:   "app: " + name,
		RenderedYAML: "services: {}",
	}
	if err := st.RecordDeploy(context.Background(), rev, ""); err != nil {
		t.Fatalf("seed stack %q: %v", name, err)
	}
}

// wantScoped403 asserts the exact refusal a scoped token gets outside its
// stack: HTTP 403 with {"error":"token scoped to stack demo"}.
func wantScoped403(t *testing.T, resp *http.Response, msg string) {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("%s: status = %d, want 403 (body: %s)", msg, resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("%s: Content-Type = %q, want application/json", msg, ct)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("%s: decode body %q: %v", msg, raw, err)
	}
	if body["error"] != "token scoped to stack demo" {
		t.Errorf("%s: error = %v, want %q", msg, body["error"], "token scoped to stack demo")
	}
}

// wantStatus asserts a status and fails with the body attached.
func wantStatus(t *testing.T, resp *http.Response, want int, msg string) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != want {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: status = %d, want %d (body: %s)", msg, resp.StatusCode, want, raw)
	}
}

// TestStackScopedTokenHTTP drives the fully-wired server with a real
// store-minted token: its own stack's deploy/rollback/service endpoints
// answer, everything else answers 403, and an unscoped token still reaches
// all of it.
func TestStackScopedTokenHTTP(t *testing.T) {
	env := newScopeEnv(t)
	base := env.srv.URL

	t.Run("scoped token: own stack passes", func(t *testing.T) {
		cases := []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, "/api/stacks/demo", nil},
			{http.MethodGet, "/api/stacks/demo/revisions/1700000000", nil},
			{http.MethodPost, "/api/stacks/demo/sync", nil},
			{http.MethodPost, "/api/stacks/demo/rollback", map[string]any{"revision": 1700000000}},
			{http.MethodDelete, "/api/stacks/demo", nil},
			{http.MethodGet, "/api/services/demo", nil},
			{http.MethodGet, "/api/services/demo/web/tasks", nil},
			{http.MethodGet, "/api/services/demo/web/logs?tail=10", nil},
			{http.MethodPost, "/api/services/demo/web/restart", nil},
			{http.MethodPost, "/api/services/demo/web/exec", map[string]any{"argv": []string{"whoami"}}},
			{http.MethodPost, "/api/stacks", map[string]any{"app_name": "demo", "manifest": "app: demo"}},
			{http.MethodPost, "/api/stacks", map[string]any{"manifest": "app: demo"}},
		}
		for _, tc := range cases {
			resp := doJSON(t, tc.method, base+tc.path, env.scoped, tc.body)
			wantStatus(t, resp, http.StatusOK, tc.method+" "+tc.path)
		}
	})

	t.Run("scoped token: other stacks and non-stack routes → 403", func(t *testing.T) {
		cases := []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, "/api/stacks", nil},
			{http.MethodGet, "/api/stacks/other", nil},
			{http.MethodPost, "/api/stacks/other/sync", nil},
			{http.MethodPost, "/api/stacks/other/rollback", map[string]any{"revision": 1700000000}},
			{http.MethodDelete, "/api/stacks/other", nil},
			{http.MethodGet, "/api/services", nil},
			{http.MethodGet, "/api/services/other", nil},
			{http.MethodGet, "/api/services/other/web/tasks", nil},
			{http.MethodPost, "/api/services/other/web/restart", nil},
			{http.MethodPost, "/api/stacks", map[string]any{"app_name": "other", "manifest": "app: other"}},
			{http.MethodPost, "/api/stacks", map[string]any{"manifest": "app: other"}},
			{http.MethodGet, "/api/cluster/settings", nil},
			{http.MethodGet, "/api/me", nil},
			{http.MethodGet, "/api/api_keys", nil},
		}
		for _, tc := range cases {
			resp := doJSON(t, tc.method, base+tc.path, env.scoped, tc.body)
			wantScoped403(t, resp, tc.method+" "+tc.path)
		}
	})

	t.Run("unscoped token: everything still passes", func(t *testing.T) {
		cases := []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, "/api/me", nil},
			{http.MethodGet, "/api/cluster/settings", nil},
			{http.MethodGet, "/api/stacks", nil},
			{http.MethodGet, "/api/stacks/other", nil},
			{http.MethodPost, "/api/stacks/other/sync", nil},
			{http.MethodGet, "/api/services", nil},
			{http.MethodGet, "/api/services/other", nil},
			{http.MethodGet, "/api/api_keys", nil},
			{http.MethodPost, "/api/stacks", map[string]any{"app_name": "other", "manifest": "app: other"}},
		}
		for _, tc := range cases {
			resp := doJSON(t, tc.method, base+tc.path, env.unscoped, tc.body)
			wantStatus(t, resp, http.StatusOK, tc.method+" "+tc.path)
		}
	})

	t.Run("missing token: still 401", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base+"/api/stacks/demo", "", nil)
		wantStatus(t, resp, http.StatusUnauthorized, "GET /api/stacks/demo unauthenticated")
	})
}

// TestAPIKeyCreateHTTPWithStack covers the token-minting surface end to end:
// POST /api/api_keys accepts a stack scope, echoes it back, and the minted
// token really is confined to that stack.
func TestAPIKeyCreateHTTPWithStack(t *testing.T) {
	srv := newKeysServer(t)
	base := srv.URL + "/api/api_keys"

	resp := doJSON(t, http.MethodPost, base, "tok", map[string]string{"name": "deployer", "stack": "demo"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("create = %d, want 201 (body: %s)", resp.StatusCode, raw)
	}
	created := decodeBody[struct {
		Name  string `json:"name"`
		Stack string `json:"stack"`
		Token string `json:"token"`
	}](t, resp)
	if created.Stack != "demo" {
		t.Errorf("created stack = %q, want demo", created.Stack)
	}
	if created.Token == "" {
		t.Fatal("created token is empty")
	}

	list := decodeBody[struct {
		Keys []struct {
			Name  string `json:"name"`
			Stack string `json:"stack"`
		} `json:"keys"`
	}](t, doJSON(t, http.MethodGet, base, "tok", nil))
	found := false
	for _, k := range list.Keys {
		if k.Name == "deployer" {
			found = true
			if k.Stack != "demo" {
				t.Errorf("listed stack = %q, want demo", k.Stack)
			}
		}
	}
	if !found {
		t.Fatal("created key missing from list")
	}
}
