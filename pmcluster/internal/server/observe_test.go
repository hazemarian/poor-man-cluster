package server

import (
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/services"
	"net/http/httptest"
	"testing"
)

func TestObserveDeniesWorkloadMutations(t *testing.T) {
	h := New(Deps{Lookup: investigationLookup{}, Services: &services.Standalone{}, ReadOnly: true})
	for _, path := range []string{"/api/services/demo/web/restart", "/api/services/demo/web/exec", "/api/nodes/host/storage", "/api/stacks/demo", "/api/cluster/settings"} {
		for _, method := range []string{"POST", "PUT", "DELETE"} {
			r := httptest.NewRequest(method, path, nil)
			r.Header.Set("Authorization", "Bearer admin")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 405 {
				t.Fatalf("%s %s=%d", method, path, w.Code)
			}
		}
	}
	r := httptest.NewRequest("GET", "/api/services/demo/web/exec/ws", nil)
	r.Header.Set("Authorization", "Bearer admin")
	r.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == 101 {
		t.Fatal("exec websocket available")
	}
	r = httptest.NewRequest("GET", "/api/services", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("unauth=%d", w.Code)
	}
}
