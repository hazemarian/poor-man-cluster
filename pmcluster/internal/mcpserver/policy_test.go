package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	pmcdocs "github.com/hazemarian/poor-man-cluster/pmcluster/docs"
)

// withDefaults builds the server with Options.Spec set and AllowOps untouched,
// which is what serve.go does when PMCLUSTER_MCP_WRITES is unset.
func withDefaults(t *testing.T, stub *stubRouter) Options {
	t.Helper()
	return Options{Spec: pmcdocs.OpenAPI}
}

func callJSON(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned an error: %s", name, firstText(t, res))
	}
	text := firstText(t, res)
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s did not answer JSON: %v (%s)", name, err, text)
	}
	return out
}

// The one permitted write lands, with the caller's token, as a config content
// update against the real route.
func TestDefaultPolicyAllowsServiceConfigUpdate(t *testing.T) {
	stub := &stubRouter{
		byPath: map[string]string{
			"GET /api/configs/app-cfg": `{"id":7,"scope":"service","stack":"shop","name":"app-cfg","kind":"env","version":"v1","hash":"h1","content":"A=1"}`,
			"PUT /api/configs/app-cfg": `{"name":"app-cfg","hash":"h2"}`,
		},
		body: `{"name":"app-cfg","hash":"h2"}`,
	}
	cs := connect(t, stub, withDefaults(t, stub), "Bearer pmc_token")
	out := callJSON(t, cs, "pmc_update_config", map[string]any{"name": "app-cfg", "content": "A=2"})
	if out["hash"] != "h2" {
		t.Errorf("update did not return the new hash: %v", out)
	}

	var put *recordedCall
	for i, c := range stub.snapshot() {
		if c.Method == http.MethodPut {
			put = &stub.snapshot()[i]
		}
	}
	if put == nil {
		t.Fatalf("no PUT was dispatched: %v", stub.snapshot())
	}
	if put.Path != "/api/configs/app-cfg" {
		t.Errorf("PUT path = %q", put.Path)
	}
	if put.Auth != "Bearer pmc_token" {
		t.Errorf("PUT carried %q, want the caller's token", put.Auth)
	}
	if !strings.Contains(put.Body, `"content":"A=2"`) {
		t.Errorf("PUT body = %s, want the new content", put.Body)
	}
}

// A cluster-scope config is the platform's own material: the tool reads the
// config first and refuses without issuing a write.
func TestClusterScopeConfigIsRefused(t *testing.T) {
	stub := &stubRouter{
		byPath: map[string]string{
			"GET /api/configs/otel-config": `{"id":1,"scope":"cluster","name":"otel-config","kind":"template","content":"x"}`,
		},
	}
	cs := connect(t, stub, withDefaults(t, stub), "Bearer t")
	out := callJSON(t, cs, "pmc_update_config", map[string]any{"name": "otel-config", "content": "y"})
	if out["status"] != "manual_action_required" {
		t.Fatalf("cluster-scope update was not refused: %v", out)
	}
	for _, c := range stub.snapshot() {
		if c.Method == http.MethodPut {
			t.Errorf("a cluster-scope config was written despite the refusal")
		}
	}
}

// Every other write is refused by default — deploy, restart, settings, backups.
func TestOnlyConfigUpdateIsPermittedByDefault(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
	}{
		{"pmc_deploy", map[string]any{"manifest": "app: x"}},
		{"pmc_restart_service", map[string]any{"stack": "shop", "service": "shop_web"}},
		{"pmc_update_cluster_settings", map[string]any{"settings": map[string]any{"domain": "x.example"}}},
		{"pmc_create_backup", map[string]any{}},
		{"pmc_delete_config", map[string]any{"name": "app-cfg"}},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			stub := &stubRouter{}
			cs := connect(t, stub, withDefaults(t, stub), "Bearer t")
			out := callJSON(t, cs, tc.tool, tc.args)
			if out["status"] != "manual_action_required" {
				t.Fatalf("%s was not refused: %v", tc.tool, out)
			}
			if out["do_not_retry"] != true {
				t.Errorf("%s refusal does not say do_not_retry", tc.tool)
			}
			if len(stub.snapshot()) != 0 {
				t.Errorf("%s reached the router despite being refused", tc.tool)
			}
		})
	}
}

// Even with every non-dangerous write enabled, the dangerous tier stays manual.
func TestDangerousStaysManualEvenWhenAllWritesEnabled(t *testing.T) {
	all, err := AllWrites(pmcdocs.OpenAPI)
	if err != nil {
		t.Fatalf("AllWrites: %v", err)
	}
	if !all["POST /api/services/{stack}/{service}/restart"] {
		t.Fatal("AllWrites should include service restart")
	}
	for _, key := range []string{
		"POST /api/services/{stack}/{service}/exec",
		"GET /api/secrets/{name}/value",
		"DELETE /api/stacks/{name}",
		"POST /api/backups/{id}/restore",
	} {
		if all[key] {
			t.Errorf("AllWrites must never contain the dangerous operation %q", key)
		}
	}

	stub := &stubRouter{}
	cs := connect(t, stub, Options{Spec: pmcdocs.OpenAPI, AllowOps: all}, "Bearer t")
	out := callJSON(t, cs, "pmc_exec_service", map[string]any{"stack": "shop", "service": "shop_web", "argv": []any{"id"}})
	if out["status"] != "manual_action_required" {
		t.Fatalf("exec was served despite being dangerous: %v", out)
	}
	if len(stub.snapshot()) != 0 {
		t.Errorf("exec reached the router")
	}

	// …while an ordinary write now goes through.
	if res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "pmc_restart_service",
		Arguments: map[string]any{"stack": "shop", "service": "shop_web"},
	}); err != nil || res.IsError {
		t.Fatalf("restart should be permitted when explicitly allowed: %v", err)
	}
}

// The capability tool has to describe this policy, because that is what an agent
// reads before deciding whether to ask the user for help.
func TestCapabilitiesDescribeTheNarrowPolicy(t *testing.T) {
	cs := connect(t, &stubRouter{}, withDefaults(t, &stubRouter{}), "Bearer t")
	out := callJSON(t, cs, "pmc_capabilities", nil)

	allowed, _ := out["writes_allowed"].([]any)
	if len(allowed) != 1 || allowed[0] != "pmc_update_config" {
		t.Errorf("writes_allowed = %v, want exactly [pmc_update_config]", allowed)
	}
	refused, _ := out["writes_refused"].([]any)
	var sawDeploy, sawExec bool
	for _, r := range refused {
		switch r {
		case "pmc_deploy":
			sawDeploy = true
		case "pmc_exec_service":
			sawExec = true
		}
	}
	if !sawDeploy || !sawExec {
		t.Errorf("writes_refused does not name deploy and exec: %v", refused)
	}
}
