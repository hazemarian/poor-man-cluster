package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
)

// The MCP surface is a sub-route of the API, so it inherits the daemon's auth
// chain: an unauthenticated request is refused before any tool is reachable, and
// an authenticated one gets the same catalog the mcpserver package unit-tests.
func TestMCPSubRouteIsBehindTheAPIToken(t *testing.T) {
	srv := httptest.NewServer(New(Deps{
		Lookup: &fakeLookup{users: map[string]*auth.User{
			"tok": {ID: 1, Name: "admin", Role: auth.RoleAdmin},
		}},
		MCP: &MCPOptions{AllowOps: map[string]bool{}},
	}))
	defer srv.Close()

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}`

	// (1) no token → the daemon's own 401, never a tool call.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/mcp", strings.NewReader(initBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unauthenticated request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/mcp = %d, want 401", resp.StatusCode)
	}

	// (2) with a token → a real JSON-RPC handshake.
	session, payload, status := mcpPost(t, srv.URL, "tok", initBody, "")
	if status != http.StatusOK {
		t.Fatalf("initialize = %d: %s", status, payload)
	}
	if !strings.Contains(payload, "protocolVersion") {
		t.Fatalf("initialize answered without protocolVersion: %s", payload)
	}

	// (3) the catalog is served over the wire.
	notif := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	if _, _, status := mcpPost(t, srv.URL, "tok", notif, session); status != http.StatusAccepted && status != http.StatusOK {
		t.Fatalf("initialized notification = %d", status)
	}
	listBody := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	_, payload, status = mcpPost(t, srv.URL, "tok", listBody, session)
	if status != http.StatusOK {
		t.Fatalf("tools/list = %d: %s", status, payload)
	}
	for _, want := range []string{"pmc_list_stacks", "pmc_cluster_status", "pmc_manual_action"} {
		if !strings.Contains(payload, want) {
			t.Errorf("tools/list is missing %s", want)
		}
	}
}

// mcpPost sends one JSON-RPC message and returns the session id (when the server
// issues one) plus the response body with any SSE framing stripped.
func mcpPost(t *testing.T, base, token, body, session string) (string, string, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/api/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", body, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.Header.Get("Mcp-Session-Id"), stripSSE(string(raw)), resp.StatusCode
}

// stripSSE returns the JSON payload of an SSE stream, or the body unchanged.
func stripSSE(body string) string {
	if !strings.HasPrefix(strings.TrimSpace(body), "event:") && !strings.Contains(body, "data: ") {
		return body
	}
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			out = append(out, strings.TrimPrefix(line, "data: "))
		}
	}
	return strings.Join(out, "\n")
}
