package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/configs"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// openServerStore opens a fresh store in a temp dir for one test.
func openServerStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// doJSON performs a JSON request against the server with an optional bearer
// token and optional body, returning the raw response.
func doJSON(t *testing.T, method, url, token string, body any) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// TestRenderedConfigsAPI exercises GET /api/cluster/rendered — served by the
// configs handler via ListRendered: auth and the read-only listing of the
// rendered platform configs snapshotted by cluster update.
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
		Lookup:  &fakeLookup{users: map[string]*auth.User{"tok": {ID: 1, Name: "admin"}}},
		Configs: configs.NewLocal(st),
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
// Configs dependency the rendered route is omitted.
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
