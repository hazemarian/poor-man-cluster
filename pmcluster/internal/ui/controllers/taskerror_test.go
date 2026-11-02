package controllers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/middleware"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/views"
)

// A service can be deployed, recorded, and still have no container at all — an
// unpullable image, a rejected task. The console showed "0 / 1" and stopped
// there; the reason lived only behind the per-service Tasks tab, which nobody
// opens when the page gives them nothing to look for.
//
// Observed: adam-world-dev was green in CI, `0/1` in the console, and the actual
// answer ("failed to resolve reference … not found") appeared nowhere on the
// stack page.
func TestServiceTaskErrorIsShownOnTheList(t *testing.T) {
	var taskCalls atomic.Int32
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"settings":{"domain":"example.test"}}`)
	})
	mux.HandleFunc("/api/services", func(w http.ResponseWriter, r *http.Request) {
		write(w, `{"services":[
			{"stack":"broke","name":"web","image":"ghcr.io/org/app:dev","replicas":0,"desired":1,"mode":"replicated","update_state":"completed"},
			{"stack":"fine","name":"web","image":"nginx:1.27","replicas":1,"desired":1,"mode":"replicated","update_state":"completed"}
		]}`)
	})
	mux.HandleFunc("/api/services/broke/web/tasks", func(w http.ResponseWriter, r *http.Request) {
		taskCalls.Add(1)
		write(w, `{"tasks":[
			{"task_id":"t2","slot":1,"state":"rejected","error":"failed to resolve reference \"ghcr.io/org/app-dev:dev-v1\": not found"},
			{"task_id":"t1","slot":2,"state":"shutdown","error":""}
		]}`)
	})
	mux.HandleFunc("/api/services/fine/web/tasks", func(w http.ResponseWriter, r *http.Request) {
		taskCalls.Add(1)
		write(w, `{"tasks":[{"task_id":"t1","slot":1,"state":"running","error":""}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	renderer, err := views.NewRenderer()
	if err != nil {
		t.Fatalf("renderer: %v", err)
	}
	renderer.ShellData = views.ShellBase
	ctrl := &Controller{
		Store: st, Views: renderer,
		Auth: middleware.NewAuth(st, []byte("0123456789abcdefgh"), "pmui_session"),
		API:  pmapi.New(srv.URL, "pmc_test", 5*1000*1000*1000),
		// The console considers itself configured when it has a daemon to ask;
		// without these it renders the "not configured" fragment instead.
		EnvAPI: srv.URL, EnvToken: "pmc_test",
	}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/web/services", (Services{Controller: ctrl}).List)

	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://x/web/services", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	if !strings.Contains(body, "failed to resolve reference") {
		t.Errorf("the failing service's reason is not on the page:\n%s", body)
	}
	if !strings.Contains(body, "not found") {
		t.Errorf("error text truncated past the useful part")
	}
	if got := taskCalls.Load(); got != 1 {
		t.Errorf("task lookups = %d, want 1 — only the non-converged service is asked", got)
	}
	// The healthy service must not borrow the failing one's error: the page
	// renders the reason once per row variant (table + stacked card), so count the
	// rows that carry it by their label instead.
	if !strings.Contains(body, "not found") {
		t.Errorf("the reason is truncated past the useful part")
	}
	labels := strings.Count(body, "Task error:")
	if labels == 0 || labels > 2 {
		t.Errorf("task-error labels = %d, want 1 per row variant (table + card)", labels)
	}
}
