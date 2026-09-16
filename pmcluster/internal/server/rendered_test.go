package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
)

// TestRenderedConfigsAPI exercises GET /api/cluster/rendered: auth and the
// read-only listing of the rendered platform configs snapshotted by cluster
// update.
func TestRenderedConfigsAPI(t *testing.T) {
	st := openServerStore(t)
	ctx := context.Background()
	for name, content := range map[string]string{
		"infra-stack":     "version: \"3.9\"\nservices: {}",
		"traefik-dynamic": "tls:\n  certificates: []\n",
	} {
		if _, err := st.CreateConfig(ctx, "cluster", "", name, "template", content, "v0.2.39"); err != nil {
			t.Fatalf("CreateConfig(%s): %v", name, err)
		}
		if err := st.SetRendered(ctx, name, content); err != nil {
			t.Fatalf("SetRendered(%s): %v", name, err)
		}
	}

	srv := httptest.NewServer(New(Deps{
		Lookup:   &fakeLookup{users: map[string]*auth.User{"tok": {ID: 1, Name: "admin"}}},
		Rendered: &RenderedConfigService{Store: st},
	}))
	defer srv.Close()
	url := srv.URL + "/api/cluster/rendered"

	if resp := doJSON(t, http.MethodGet, url, "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth GET = %d, want 401", resp.StatusCode)
	}

	resp := doJSON(t, http.MethodGet, url, "tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body: %s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	for _, want := range []string{`"name":"infra-stack"`, `"name":"traefik-dynamic"`, "tls:", "certificates: []"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered response missing %q; got: %s", want, body)
		}
	}
	if strings.Index(body, `"name":"infra-stack"`) > strings.Index(body, `"name":"traefik-dynamic"`) {
		t.Errorf("rendered configs not sorted alphabetically; got: %s", body)
	}
}

// TestRenderedConfigsAPI_NotWired covers the optional-service pattern: with no
// Rendered dependency the route is omitted.
func TestRenderedConfigsAPI_NotWired(t *testing.T) {
	srv := httptest.NewServer(New(Deps{
		Lookup: &fakeLookup{users: map[string]*auth.User{"tok": {ID: 1, Name: "admin"}}},
	}))
	defer srv.Close()
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/cluster/rendered", "tok", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET (not wired) = %d, want 404 (route omitted)", resp.StatusCode)
	}
}

// TestRenderedConfigsAPI_NoStore covers the internal guard when the service is
// mounted without a store.
func TestRenderedConfigsAPI_NoStore(t *testing.T) {
	srv := httptest.NewServer(New(Deps{
		Lookup:   &fakeLookup{users: map[string]*auth.User{"tok": {ID: 1, Name: "admin"}}},
		Rendered: &RenderedConfigService{},
	}))
	defer srv.Close()
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/cluster/rendered", "tok", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("GET (no store) = %d, want 500", resp.StatusCode)
	}
	if !strings.Contains(readBody(t, resp), "rendered-config service not wired") {
		t.Errorf("expected wiring error message")
	}
}
