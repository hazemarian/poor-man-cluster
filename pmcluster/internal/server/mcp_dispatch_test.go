package server

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/settings"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/usage"
)

// mcpInitialize performs the JSON-RPC handshake and returns the session id.
func mcpInitialize(t *testing.T, base, token string) string {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}`
	session, payload, status := mcpPost(t, base, token, body, "")
	if status != 200 {
		t.Fatalf("initialize: %d %s", status, payload)
	}
	notif := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	if _, _, code := mcpPost(t, base, token, notif, session); code != 202 && code != 200 {
		t.Fatalf("initialized notification: %d", code)
	}
	return session
}

// mcpTestServer is a daemon with the routes a store alone can serve.
func mcpTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ts := httptest.NewServer(New(Deps{
		Lookup:   &fakeLookup{users: map[string]*auth.User{"tok": {ID: 1, Name: "admin", Role: auth.RoleAdmin}}},
		Store:    st,
		Settings: settings.NewLocal(st),
		Usage:    usage.NewLocal(st),
		MCP:      &MCPOptions{},
	}))
	t.Cleanup(ts.Close)
	return ts
}

// A derived read must survive the whole path: MCP tools/call → in-process
// dispatch → the real chi router → the handler.
//
// This is the test whose absence shipped v0.2.219 with every read broken:
// initialize and tools/list never dispatch, and the mcpserver tests use a stub
// router that has no chi in it. The defect lived exactly in between — chi's
// Mux.ServeHTTP finds a RouteCtxKey in the request context and delegates to its
// handler WITHOUT routing, so the sub-request inherited /api/mcp: curated reads
// got a 400/405 out of the MCP handler re-entering itself, derived reads crashed
// it (a recovered panic, logged as a bare 500).
func TestMCPToolCallDispatchesThroughTheRouter(t *testing.T) {
	ts := mcpTestServer(t)
	session := mcpInitialize(t, ts.URL, "tok")

	for _, tool := range []string{"pmc_get_cluster_settings", "pmc_get_usage"} {
		body := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"` + tool + `","arguments":{}}}`
		_, payload, status := mcpPost(t, ts.URL, "tok", body, session)
		if status != 200 {
			t.Fatalf("%s: transport %d: %s", tool, status, payload)
		}
		if strings.Contains(payload, `"isError":true`) {
			t.Fatalf("%s: dispatch failed: %s", tool, payload)
		}
		if !strings.Contains(payload, `"content"`) {
			t.Fatalf("%s: no content in the result: %s", tool, payload)
		}
	}
}

// A curated tool dispatches several reads and must report the sources it could not
// reach — not fail as a whole, and not report the old routing error.
func TestMCPCuratedToolDispatchesThroughTheRouter(t *testing.T) {
	ts := mcpTestServer(t)
	session := mcpInitialize(t, ts.URL, "tok")

	body := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"pmc_cluster_capacity","arguments":{}}}`
	_, payload, status := mcpPost(t, ts.URL, "tok", body, session)
	if status != 200 {
		t.Fatalf("transport %d: %s", status, payload)
	}
	if strings.Contains(payload, `"isError":true`) {
		t.Fatalf("curated tool failed outright: %s", payload)
	}
	// /api/services and /api/cluster/info are not mounted here (no Docker, no
	// services service), so the answer must name them as unavailable sources.
	if !strings.Contains(payload, "unavailable_sources") {
		t.Fatalf("degraded answer does not name its unavailable sources: %s", payload)
	}
	if !strings.Contains(payload, "404") {
		t.Fatalf("an unmounted source should read as 404, not the old routing error: %s", payload)
	}
	if strings.Contains(payload, "405") {
		t.Fatalf("405 is back — the sub-request is being routed as /api/mcp again: %s", payload)
	}
}
