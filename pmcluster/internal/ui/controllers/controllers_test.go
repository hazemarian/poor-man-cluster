package controllers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/middleware"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/views"
)

// ctrlFakeDaemon serves canned /api responses for the controllers under test.
// Every endpoint requires the Bearer token, mirroring the daemon.
func ctrlFakeDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body) //nolint:errcheck
	}
	auth := func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer pmc_test"
	}

	mux.HandleFunc("/api/me", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		write(w, `{"id":1,"name":"cli","role":"operator"}`)
	})
	mux.HandleFunc("/api/cluster/info", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"node_name":"mgr","server_version":"28","os":"linux","arch":"amd64","cpus":8,"memory_bytes":8589934592,"swarm":{"state":"active","control_available":true,"managers":1,"nodes":2}}`)
	})
	mux.HandleFunc("/api/nodes", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"nodes":[{"hostname":"manager-1","role":"manager","status":"ready","is_leader":true,"engine_version":"28.5","storage":true},{"hostname":"worker-1","role":"worker","status":"ready","engine_version":"28.5","storage":false}]}`)
	})
	mux.HandleFunc("/api/nodes/manager-1/storage", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			write(w, `{"hostname":"manager-1","storage":true,"storage_nodes":"manager-1"}`)
		case http.MethodDelete:
			write(w, `{"hostname":"manager-1","storage":false,"storage_nodes":""}`)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/nodes/worker-1/storage", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			write(w, `{"hostname":"worker-1","storage":true,"storage_nodes":"manager-1,worker-1"}`)
		case http.MethodDelete:
			write(w, `{"hostname":"worker-1","storage":false,"storage_nodes":"manager-1"}`)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/stacks", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"stacks":[{"name":"demo","current_revision":3,"repo_url":"https://example.com/demo","created_at":1,"updated_at":2}]}`)
	})
	mux.HandleFunc("/api/stacks/demo", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"stack":{"name":"demo","current_revision":3,"repo_url":"https://example.com/demo","last_error":[{"revision":3,"error":"boom","created_at":20}]},"revisions":[{"revision":3,"created_at":30},{"revision":2,"created_at":20}],"last_backup":{"status":"succeeded","started_at":25}}`)
	})
	mux.HandleFunc("/api/stacks/demo/revisions/3", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"stack":"demo","revision":3,"created_at":30,"source_yaml":"app: demo\n","rendered_yaml":"services:\n  demo:\n","payload":"{}"}`)
	})
	mux.HandleFunc("/api/stacks/demo/backups", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"backups":[{"id":7,"stack_name":"demo","status":"succeeded","started_at":25,"archive_path":"/archive/x.tar.gz"}]}`)
	})
	mux.HandleFunc("/api/stacks/demo/rollback", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"stack":"demo","new_revision":3,"rolled_back_to":2}`)
	})
	mux.HandleFunc("/api/stacks/demo/sync", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"stack":"demo","revision":3,"changed":false}`)
	})
	mux.HandleFunc("/api/stacks/demo/move", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"stack":"demo","moved_to":"node-02"}`)
	})
	mux.HandleFunc("/api/services", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"services":[{"stack":"demo","name":"web","image":"nginx:1.27","replicas":2,"desired":2,"mode":"replicated","updated":30,"node":"node-01","update_state":"completed"}]}`)
	})
	mux.HandleFunc("/api/services/demo", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"services":[{"stack":"demo","name":"web","image":"nginx:1.27","replicas":2,"desired":2,"mode":"replicated","updated":30,"node":"node-01","update_state":"completed"}]}`)
	})
	mux.HandleFunc("/api/services/demo/web/tasks", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"tasks":[{"task_id":"t1","slot":1,"state":"running","error":"","node":"node-01","started_at":30,"finished_at":0}]}`)
	})
	mux.HandleFunc("/api/services/demo/web/logs", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"logs":[{"stream":"stdout","line":"hello"}]}`)
	})
	mux.HandleFunc("/api/backups", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"backups":[{"id":7,"stack_name":"demo","status":"succeeded","started_at":25,"archive_path":"/archive/x.tar.gz"}]}`)
	})
	mux.HandleFunc("/api/backups/7/files", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"run":{"id":7,"stack_name":"demo","status":"succeeded"},"files":[{"path":"backup/data/demo/web.txt","size":5}]}`)
	})
	mux.HandleFunc("/api/api_keys", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			write(w, `{"keys":[{"id":1,"name":"ci","created_at":10,"last_used_at":20}]}`)
		case http.MethodPost:
			write(w, `{"id":2,"name":"ci2","token":"pmc_abcdef_restofsecret"}`)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/api_keys/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/webhooks", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"hooks":[{"source":"deploy","url":"https://example.com/hook","description":"d","created_at":1}]}`)
	})
	mux.HandleFunc("/api/webhooks/deploy/deliveries", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"deliveries":[{"id":1,"status":"ok","attempts":1,"http_status":200,"created_at":10}]}`)
	})
	mux.HandleFunc("/api/configs", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"configs":[{"id":1,"scope":"cluster","stack":"","name":"main","kind":"yaml","content":"{}","hash":"h1","created_at":1,"updated_at":2}]}`)
	})
	mux.HandleFunc("/api/configs/main", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"id":1,"scope":"cluster","stack":"","name":"main","kind":"yaml","content":"{}","hash":"h1","created_at":1,"updated_at":2}`)
	})
	mux.HandleFunc("/api/secrets", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"secrets":[{"id":1,"scope":"cluster","stack":"","name":"pw","hash":"h1","created_at":1,"updated_at":2}]}`)
	})
	mux.HandleFunc("/api/secrets/pw", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"value":"secret","hash":"h1"}`)
	})
	mux.HandleFunc("/api/cluster/settings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			write(w, `{"settings":{"domain":"example.test"}}`)
			return
		}
		write(w, `{"settings":{"domain":"example.test","reconcile_interval":"60"}}`)
	})
	mux.HandleFunc("/api/usage", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"stacks":2,"services":3,"backups":4,"last_backup_at":25}`)
	})
	mux.HandleFunc("/api/certs", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"hosts":[{"host":"a.example.test","valid":true,"expires_at":10}],"site":{"cn":"example.test"}}`)
	})
	mux.HandleFunc("/api/rendered", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"configs":[{"name":"pmcluster_traefik_dynamic","content":"{}","updated_at":5}]}`)
	})
	mux.HandleFunc("/api/inventory", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"stacks":[{"name":"demo","images":["nginx:1.27"],"envs":[],"updated_at":2}]}`)
	})
	mux.HandleFunc("/api/update", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"changed":false,"message":"no changes"}`)
	})
	return httptest.NewServer(mux)
}

// newCtrlHarness builds a Controller wired to a fake daemon and a real store /
// renderer / auth, then returns a gin engine with select handlers mounted.
type ctrlHarness struct {
	engine *gin.Engine
	ctrl   *Controller
}

func newCtrlHarness(t *testing.T) *ctrlHarness {
	t.Helper()
	daemon := ctrlFakeDaemon(t)
	t.Cleanup(daemon.Close)

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	renderer, err := views.NewRenderer()
	if err != nil {
		t.Fatalf("renderer: %v", err)
	}
	renderer.ShellData = views.ShellBase
	auth := middleware.NewAuth(st, []byte("0123456789abcdefgh"), "pmui_session")
	api := pmapi.New(daemon.URL, "pmc_test", 5*1000*1000*1000)

	ctrl := &Controller{
		Store: st, API: api, Views: renderer, Auth: auth,
		Version: "test", Domain: "example.test",
		EnvAPI: daemon.URL, EnvToken: "pmc_test",
	}
	engine := gin.New()
	return &ctrlHarness{engine: engine, ctrl: ctrl}
}

// get runs a handler against the harness engine and returns the recorder.
func (h *ctrlHarness) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://x"+path, nil)
	rr := httptest.NewRecorder()
	h.engine.ServeHTTP(rr, req)
	return rr
}

func (h *ctrlHarness) post(t *testing.T, path, form string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://x"+path, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.engine.ServeHTTP(rr, req)
	return rr
}

func (h *ctrlHarness) mountOverview() {
	h.engine.GET("/web/overview", (Overview{Controller: h.ctrl}).Fragment)
}
func (h *ctrlHarness) mountNodes() {
	h.engine.POST("/web/nodes/:hostname/storage", (Nodes{Controller: h.ctrl}).Promote)
	h.engine.DELETE("/web/nodes/:hostname/storage", (Nodes{Controller: h.ctrl}).Demote)
}
func (h *ctrlHarness) mountStacks() {
	h.engine.GET("/web/stacks", (Stacks{Controller: h.ctrl}).List)
	h.engine.GET("/web/stacks/:name", (Stacks{Controller: h.ctrl}).Show)
	h.engine.GET("/web/stacks/:name/revisions/:rev", (Stacks{Controller: h.ctrl}).ShowRevision)
	h.engine.GET("/web/stacks/:name/backups", (Stacks{Controller: h.ctrl}).ShowBackups)
	h.engine.POST("/web/stacks/:name/move", (Stacks{Controller: h.ctrl}).Move)
}
func (h *ctrlHarness) mountServices() {
	h.engine.GET("/web/services", (Services{Controller: h.ctrl}).List)
	h.engine.GET("/web/services/:stack/:service/tasks", (Services{Controller: h.ctrl}).Tasks)
	h.engine.GET("/web/services/:stack/:service/logs", (Services{Controller: h.ctrl}).Logs)
}
func (h *ctrlHarness) mountBackups() {
	h.engine.GET("/web/backups", (Backups{Controller: h.ctrl}).List)
	h.engine.GET("/web/backups/:id/files", (Backups{Controller: h.ctrl}).Browse)
}
func (h *ctrlHarness) mountAPIKeys() {
	h.engine.GET("/web/apikeys", (APIKeys{Controller: h.ctrl}).Page)
	h.engine.POST("/web/apikeys/add", (APIKeys{Controller: h.ctrl}).Add)
	h.engine.POST("/web/apikeys/remove/:id", (APIKeys{Controller: h.ctrl}).Remove)
}
func (h *ctrlHarness) mountSettings() {
	h.engine.GET("/web/settings", (Settings{Controller: h.ctrl}).Page)
	h.engine.GET("/web/settings/rendered/:name", (Settings{Controller: h.ctrl}).RenderedGet)
}
func (h *ctrlHarness) mountWebhooks() {
	h.engine.GET("/web/webhooks", (Webhooks{Controller: h.ctrl}).Page)
	h.engine.GET("/web/webhooks/:source/deliveries", (Webhooks{Controller: h.ctrl}).Deliveries)
}
func (h *ctrlHarness) mountUsage() { h.engine.GET("/web/usage", (Usage{Controller: h.ctrl}).Page) }
func (h *ctrlHarness) mountTLS()   { h.engine.GET("/web/tls", (TLS{Controller: h.ctrl}).Page) }

func TestControllers_Overview(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountOverview()
	rr := h.get(t, "/web/overview")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /web/overview = %d, want 200", rr.Code)
	}
	if b := rr.Body.String(); !strings.Contains(b, "manager-1") || !strings.Contains(b, "mgr") {
		t.Errorf("overview missing node/info data: %s", b[:min(len(b), 200)])
	}
}

func TestControllers_Stacks(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountStacks()
	for _, tc := range []struct {
		path, want string
		code       int
	}{
		{"/web/stacks", "demo", http.StatusOK},
		{"/web/stacks/demo", "demo", http.StatusOK},
		{"/web/stacks/demo/revisions/3", "demo", http.StatusOK},
		// ShowBackups is a redirect to the global backups page.
		{"/web/stacks/demo/backups", "", http.StatusFound},
	} {
		rr := h.get(t, tc.path)
		if rr.Code != tc.code {
			t.Errorf("GET %s = %d, want %d", tc.path, rr.Code, tc.code)
			continue
		}
		if tc.want != "" && !strings.Contains(rr.Body.String(), tc.want) {
			t.Errorf("GET %s body missing %q", tc.path, tc.want)
		}
	}
	// Unknown stack → 404 rendered gracefully.
	rr := h.get(t, "/web/stacks/nope")
	if rr.Code != http.StatusOK && rr.Code != http.StatusNotFound {
		t.Errorf("GET /web/stacks/nope = %d, want 200/404", rr.Code)
	}
}

func TestControllers_StackMove(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountStacks()
	// Happy path: POST target=node-02 → daemon 200 → fragment shows the moved
	// confirmation banner.
	rr := h.post(t, "/web/stacks/demo/move", "target=node-02")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST move = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Moved demo to node-02") {
		t.Errorf("POST move body missing confirmation, got: %s", rr.Body.String())
	}
	// Empty target → validation error fragment.
	rr = h.post(t, "/web/stacks/demo/move", "target=")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST move (empty target) = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Choose a destination node") {
		t.Errorf("POST move (empty target) body missing err_move_target, got: %s", rr.Body.String())
	}
}

func TestControllers_Services(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountServices()
	for _, tc := range []struct{ path, want string }{
		{"/web/services", "nginx:1.27"},
		{"/web/services/demo/web/tasks", "running"},
		{"/web/services/demo/web/logs", "hello"},
	} {
		rr := h.get(t, tc.path)
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", tc.path, rr.Code)
			continue
		}
		if !strings.Contains(rr.Body.String(), tc.want) {
			t.Errorf("GET %s body missing %q", tc.path, tc.want)
		}
	}
}

func TestControllers_Backups(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountBackups()
	for _, path := range []string{"/web/backups", "/web/backups/7/files"} {
		rr := h.get(t, path)
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rr.Code)
		}
	}
}

func TestControllers_APIKeys(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountAPIKeys()
	if rr := h.get(t, "/web/apikeys"); rr.Code != http.StatusOK {
		t.Fatalf("GET /web/apikeys = %d, want 200", rr.Code)
	}
	// Add reveals the one-time token.
	rr := h.post(t, "/web/apikeys/add", "name=ci2")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /web/apikeys/add = %d, want 200", rr.Code)
	}
	if b := rr.Body.String(); !strings.Contains(b, "pmc_abcdef") {
		t.Errorf("add response missing one-time token: %s", b)
	}
	// Blank name refused without calling the daemon.
	if rr := h.post(t, "/web/apikeys/add", "name="); rr.Code != http.StatusOK {
		t.Errorf("POST blank name = %d, want 200 (error fragment)", rr.Code)
	}
	// Remove by id.
	if rr := h.post(t, "/web/apikeys/remove/1", ""); rr.Code != http.StatusOK {
		t.Errorf("POST remove id=1 = %d, want 200", rr.Code)
	}
	// Bad id refused.
	if rr := h.post(t, "/web/apikeys/remove/abc", ""); rr.Code != http.StatusOK {
		t.Errorf("POST remove id=abc = %d, want 200 (error fragment)", rr.Code)
	}
}

func TestControllers_Settings(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountSettings()
	for _, path := range []string{"/web/settings", "/web/settings/rendered/pmcluster_traefik_dynamic"} {
		rr := h.get(t, path)
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rr.Code)
		}
	}
}

func TestControllers_WebhooksUsageTLS(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountWebhooks()
	h.mountUsage()
	h.mountTLS()
	for _, path := range []string{
		"/web/webhooks",
		"/web/webhooks/deploy/deliveries",
		"/web/usage",
		"/web/tls",
	} {
		rr := h.get(t, path)
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rr.Code)
		}
	}
}

func TestTokenPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"pmc_abcdef_rest", "pmc_abcdef"},
		{"plainlongtokenvalue12", "plainlongtok"},
		{"short", "short"},
	}
	for _, tc := range cases {
		if got := tokenPrefix(tc.in); got != tc.want {
			t.Errorf("tokenPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestControllers_NodesPromoteDemote(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountNodes()

	// Promote worker-1 → 200 overview fragment re-render with storage pill.
	rr := h.post(t, "/web/nodes/worker-1/storage", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /web/nodes/worker-1/storage = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "worker-1") {
		t.Errorf("promote fragment missing node name; body:\n%s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "storage") {
		t.Errorf("promote fragment missing storage pill; body:\n%s", rr.Body.String())
	}

	// Demote manager-1 → 200 fragment re-render.
	req := httptest.NewRequest(http.MethodDelete, "http://x/web/nodes/manager-1/storage", nil)
	rr2 := httptest.NewRecorder()
	h.engine.ServeHTTP(rr2, req)
	if rr2.Code != http.StatusOK {
		t.Fatalf("DELETE /web/nodes/manager-1/storage = %d, want 200", rr2.Code)
	}
	if !strings.Contains(rr2.Body.String(), "manager-1") {
		t.Errorf("demote fragment missing node name; body:\n%s", rr2.Body.String())
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
