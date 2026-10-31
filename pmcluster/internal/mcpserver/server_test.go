package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	pmcdocs "github.com/hazemarian/poor-man-cluster/pmcluster/docs"
)

// stubRouter stands in for the daemon router: it records what each tool call
// asked for and answers with a fixed body.
type stubRouter struct {
	mu     sync.Mutex
	calls  []recordedCall
	status int
	body   string
	byPath map[string]string // "METHOD /path" → response body
}

type recordedCall struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   string
}

func (s *stubRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var read []byte
	if r.Body != nil {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		read = buf[:n]
	}
	s.mu.Lock()
	s.calls = append(s.calls, recordedCall{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Auth:   r.Header.Get("Authorization"),
		Body:   string(read),
	})
	status, body := s.status, s.body
	if byPath, ok := s.byPath[r.Method+" "+r.URL.Path]; ok {
		body = byPath
	}
	s.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	if body == "" {
		body = `{"ok":true}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (s *stubRouter) snapshot() []recordedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedCall(nil), s.calls...)
}

// connect wires an MCP client to the server built for one request.
func connect(t *testing.T, base http.Handler, opts Options, authz string) *mcp.ClientSession {
	t.Helper()
	derived, err := BuildCatalog(opts.Spec)
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	all := make([]Tool, 0, len(derived)+len(curatedTools()))
	all = append(all, derived...)
	all = append(all, curatedTools()...)
	req := httptest.NewRequest(http.MethodPost, "/api/mcp", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	srv := buildServer(base, opts, all, req)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func toolNames(t *testing.T, cs *mcp.ClientSession) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatalf("tools: %v", err)
		}
		out[tool.Name] = true
	}
	return out
}

// The catalog must be exactly the documented operations (minus the excluded
// ones) plus the curated tools: no tool without an endpoint, no endpoint without
// a tool — the same promise docscheck makes between spec and handlers.
func TestCatalogMatchesSpecOperations(t *testing.T) {
	derived, err := BuildCatalog(pmcdocs.OpenAPI)
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	catalog := make([]Tool, 0, len(derived)+len(curatedTools()))
	catalog = append(catalog, derived...)
	catalog = append(catalog, curatedTools()...)
	if os.Getenv("MCP_TOOLS") != "" {
		for _, tool := range catalog {
			t.Logf("TOOL %-32s %-6s %s", tool.Name, tool.Tier, tool.Method+" "+tool.Path)
		}
	}
	if len(catalog) < 50 {
		t.Fatalf("derived only %d tools — the spec parse is broken", len(catalog))
	}
	seen := map[string]string{}
	for _, tool := range catalog {
		if prev, dup := seen[tool.Name]; dup {
			t.Errorf("tool %q is produced by both %s and %s", tool.Name, prev, tool.Method+" "+tool.Path)
		}
		seen[tool.Name] = tool.Method + " " + tool.Path

		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Errorf("%s: input schema is not JSON: %v", tool.Name, err)
			continue
		}
		if schema["type"] != "object" {
			t.Errorf("%s: input schema type is %v, MCP requires object", tool.Name, schema["type"])
		}
		if strings.Contains(string(tool.InputSchema), "$ref") {
			t.Errorf("%s: input schema still contains $ref — clients cannot resolve it", tool.Name)
		}
		if tool.Description == "" {
			t.Errorf("%s: no description; the model picks tools by description", tool.Name)
		}
	}
	// Every handbook action must be reachable through the one curated tool.
	var manual bool
	for _, tool := range catalog {
		if tool.Name == "pmc_manual_action" {
			manual = true
			for action := range manualActions {
				if !strings.Contains(string(tool.InputSchema), `"`+action+`"`) {
					t.Errorf("pmc_manual_action cannot express %q", action)
				}
			}
		}
	}
	if !manual {
		t.Fatal("pmc_manual_action is missing, so bootstrap work cannot be handed back")
	}
}

func TestToolsListIncludesCurated(t *testing.T) {
	stub := &stubRouter{}
	cs := connect(t, stub, Options{Spec: pmcdocs.OpenAPI, AllowOps: map[string]bool{}}, "")
	names := toolNames(t, cs)
	for _, want := range []string{"pmc_cluster_status", "pmc_cluster_capacity", "pmc_capabilities", "pmc_manual_action", "pmc_list_stacks", "pmc_get_stack"} {
		if !names[want] {
			t.Errorf("tool %q is not served", want)
		}
	}
}

// A read tool must reach the daemon router with the caller's own token.
func TestReadToolDispatchesWithCallerToken(t *testing.T) {
	stub := &stubRouter{body: `{"stacks":[{"name":"edge"}]}`}
	cs := connect(t, stub, Options{Spec: pmcdocs.OpenAPI, AllowOps: map[string]bool{}}, "Bearer pmc_test_secret")

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "pmc_list_stacks",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %v", res.Content)
	}
	calls := stub.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected one dispatch, got %d", len(calls))
	}
	if calls[0].Method != http.MethodGet || calls[0].Path != "/api/stacks" {
		t.Errorf("dispatched %s %s, want GET /api/stacks", calls[0].Method, calls[0].Path)
	}
	if calls[0].Auth != "Bearer pmc_test_secret" {
		t.Errorf("dispatched with Authorization %q — the caller's token must be forwarded", calls[0].Auth)
	}
}

// Path and query parameters belong in the URL, not in the request body.
func TestPathAndQueryArgumentsAreNotBodyFields(t *testing.T) {
	stub := &stubRouter{body: `{"stack":"edge"}`}
	cs := connect(t, stub, Options{Spec: pmcdocs.OpenAPI, AllowOps: map[string]bool{}}, "Bearer t")
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "pmc_get_stack",
		Arguments: map[string]any{"name": "edge", "limit": 5},
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	calls := stub.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected one dispatch, got %d", len(calls))
	}
	if calls[0].Path != "/api/stacks/edge" {
		t.Errorf("path = %q, want /api/stacks/edge", calls[0].Path)
	}
	if calls[0].Body != "" {
		t.Errorf("GET carried a body: %q", calls[0].Body)
	}
}

// The default policy serves exactly one write: updating a service-scoped
// config. Every other write is refused with instructions for the operator.
func TestDefaultPolicyRefusesOtherWrites(t *testing.T) {
	stub := &stubRouter{}
	cs := connect(t, stub, Options{Spec: pmcdocs.OpenAPI}, "Bearer t")

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "pmc_deploy",
		Arguments: map[string]any{"manifest": "app: x"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("a gated write should answer with instructions, not an error: %v", res.Content)
	}
	payload := firstText(t, res)
	var got map[string]any
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("refusal is not JSON: %v (%s)", err, payload)
	}
	if got["status"] != "manual_action_required" {
		t.Errorf("status = %v, want manual_action_required", got["status"])
	}
	if got["do_not_retry"] != true {
		t.Errorf("do_not_retry = %v, want true", got["do_not_retry"])
	}
	if got["command"] == "" || got["command"] == nil {
		t.Errorf("refusal names no command: %v", got)
	}
	if len(stub.snapshot()) != 0 {
		t.Errorf("a refused write still reached the router")
	}
}

// Dangerous operations need their own tier, above plain write.
func TestDangerousNeedsDangerousTier(t *testing.T) {
	stub := &stubRouter{}
	cs := connect(t, stub, Options{Spec: pmcdocs.OpenAPI}, "Bearer t")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "pmc_exec_service",
		Arguments: map[string]any{"stack": "edge", "service": "edge_pmcluster-edge", "argv": []any{"id"}},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected a manual-action answer: %v", res.Content)
	}
	if !strings.Contains(firstText(t, res), "manual_action_required") {
		t.Errorf("dangerous call was not refused: %s", firstText(t, res))
	}
	if len(stub.snapshot()) != 0 {
		t.Errorf("a refused dangerous call reached the router")
	}
}

// Bootstrap work has no API route at all: the tool must name the command.
func TestManualActionNamesTheCommand(t *testing.T) {
	cs := connect(t, &stubRouter{}, Options{Spec: pmcdocs.OpenAPI, AllowOps: map[string]bool{}}, "Bearer t")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "pmc_manual_action",
		Arguments: map[string]any{"action": "cluster_up"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	text := firstText(t, res)
	for _, want := range []string{"manual_action_required", "pmcluster cluster up", "do_not_retry"} {
		if !strings.Contains(text, want) {
			t.Errorf("manual action payload missing %q:\n%s", want, text)
		}
	}
}

// A daemon error is relayed verbatim, not dressed up as success.
func TestDaemonErrorIsRelayed(t *testing.T) {
	stub := &stubRouter{status: http.StatusForbidden, body: `{"error":"token scoped to stack edge"}`}
	cs := connect(t, stub, Options{Spec: pmcdocs.OpenAPI, AllowOps: map[string]bool{}}, "Bearer t")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "pmc_list_stacks"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatalf("403 should surface as a tool error")
	}
	if !strings.Contains(firstText(t, res), "token scoped to stack edge") {
		t.Errorf("daemon error text lost: %s", firstText(t, res))
	}
}

func firstText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatalf("result has no text content")
	return ""
}
