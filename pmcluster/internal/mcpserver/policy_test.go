package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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

// A refusal that names a command the CLI does not have is worse than a refusal
// with no command: the agent relays it verbatim and the operator runs something
// that fails. Every mapped command must start with a real pmcluster command, and
// every refusal must carry the exact REST recipe as the always-correct fallback.
func TestRefusalsNameRealCommandsAndTheExactAPI(t *testing.T) {
	real := cliCommandTree(t)
	catalog, err := BuildCatalog(pmcdocs.OpenAPI)
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	// Two shapes of the gate: the default policy (one permitted write) and the
	// permissive one (dangerous operations still refused). Every refusal either
	// shape produces must carry a correct payload.
	configs := []Options{{}, {AllowOps: AllWritesForTest(t)}}
	seen := map[string]bool{}
	for _, opts := range configs {
		for _, tool := range catalog {
			if tool.Tier == ReadOnly || allowed(opts, tool) || seen[tool.Name] {
				continue
			}
			seen[tool.Name] = true
			payload := refusal(tool, opts)
			if payload.API == "" {
				t.Errorf("%s: refusal has no API recipe", tool.Name)
			}
			if !strings.Contains(payload.API, tool.Method) || !strings.Contains(payload.API, tool.Path) {
				t.Errorf("%s: API recipe does not name %s %s: %s", tool.Name, tool.Method, tool.Path, payload.API)
			}
			if payload.Command != "" {
				tokens := strings.Fields(payload.Command)
				if len(tokens) < 2 || tokens[0] != "pmcluster" {
					t.Errorf("%s: command %q does not start with `pmcluster`", tool.Name, payload.Command)
					continue
				}
				if !commandPathValid(real, tokens[1:]) {
					t.Errorf("%s: refusal names `%s`, which does not resolve in the CLI command tree", tool.Name, payload.Command)
				}
			}
		}
	}
	if len(seen) < 25 {
		t.Fatalf("only %d refusals checked — the catalog or the gate changed shape", len(seen))
	}
	t.Logf("checked %d refusal payloads against %d CLI command paths", len(seen), len(real))
}

// AllWritesForTest is the permissive allowlist, so the loop above visits every
// refusal path including the dangerous ones.
func AllWritesForTest(t *testing.T) map[string]bool {
	t.Helper()
	all, err := AllWrites(pmcdocs.OpenAPI)
	if err != nil {
		t.Fatalf("AllWrites: %v", err)
	}
	return all
}

// cliCommandTree parses internal/cli into the command paths `pmcluster` accepts:
// every top-level command and its subcommands, from the cobra declarations
// (`var xCmd = &cobra.Command{Use: "..."}`) and the AddCommand edges. Validating
// against the real tree is the point — a flat set of `Use:` tokens accepts
// `pmcluster create webhook` because `create` exists somewhere, which is exactly
// the invented command this check is meant to reject.
func cliCommandTree(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("../cli/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no internal/cli sources: %v", err)
	}
	useRe := regexp.MustCompile(`(?m)^var (\w+) = &cobra\.Command\{\s*\n\s*Use:\s+"([^"]+)"`)
	addRe := regexp.MustCompile(`(?m)(\w+)\.AddCommand\(([^)]*)\)`)

	useByVar := map[string]string{} // varName → first token of Use
	var sources []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		sources = append(sources, string(src))
		for _, m := range useRe.FindAllStringSubmatch(string(src), -1) {
			tok := strings.Fields(m[2])
			if len(tok) > 0 && tok[0] != "" {
				useByVar[m[1]] = tok[0]
			}
		}
	}
	children := map[string][]string{} // parent var → child vars
	for _, src := range sources {
		for _, m := range addRe.FindAllStringSubmatch(src, -1) {
			parent := m[1]
			for _, raw := range strings.Split(m[2], ",") {
				child := strings.TrimSpace(raw)
				if child != "" {
					children[parent] = append(children[parent], child)
				}
			}
		}
	}

	paths := map[string]bool{}
	var walk func(parentVar, prefix string, depth int)
	walk = func(parentVar, prefix string, depth int) {
		if depth > 4 {
			return
		}
		for _, child := range children[parentVar] {
			tok, ok := useByVar[child]
			if !ok {
				continue
			}
			full := strings.TrimSpace(prefix + " " + tok)
			if paths[full] {
				continue
			}
			paths[full] = true
			walk(child, full, depth+1)
		}
	}
	for _, root := range children["rootCmd"] {
		if tok, ok := useByVar[root]; ok {
			paths[tok] = true
			walk(root, tok, 0)
		}
	}
	if len(paths) < 20 {
		t.Fatalf("parsed only %d command paths — the cobra extraction is stale", len(paths))
	}
	return paths
}

// commandPathValid reports whether `pmcluster <tokens…>` resolves in the tree.
// Placeholders (<...>) and flags end the walk; everything before them must be a
// real command or subcommand.
func commandPathValid(paths map[string]bool, tokens []string) bool {
	var walked []string
	for _, tok := range tokens {
		if strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "<") || strings.HasPrefix(tok, "[") {
			break
		}
		walked = append(walked, tok)
		if !paths[strings.Join(walked, " ")] {
			return false
		}
	}
	return len(walked) > 0
}

// The tool list itself must state the permission. A model that discovers "I may
// not write here" only by failing has spent a turn and told the user nothing; the
// description is the one place it reads before deciding.
func TestToolDescriptionsStateThePolicy(t *testing.T) {
	stub := &stubRouter{}
	cs := connect(t, stub, Options{Spec: pmcdocs.OpenAPI}, "Bearer t")
	descs := map[string]string{}
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatalf("tools: %v", err)
		}
		descs[tool.Name] = tool.Description
	}

	checks := []struct {
		tool string
		want string
		why  string
	}{
		{"pmc_update_config", "WRITE: permitted", "the one allowed write must say so"},
		{"pmc_deploy", "WRITE: NOT permitted", "an unpermitted write must say so up front"},
		{"pmc_create_webhook", "WRITE: NOT permitted", "credential minting is refused, and the tool must not pretend otherwise"},
		{"pmc_exec_service", "DANGEROUS: never served", "the dangerous tier is unreachable, stated in the description"},
	}
	for _, c := range checks {
		if !strings.Contains(descs[c.tool], c.want) {
			t.Errorf("%s: description lacks %q — %s\ngot: %s", c.tool, c.want, c.why, descs[c.tool])
		}
	}
	for _, read := range []string{"pmc_list_stacks", "pmc_cluster_status", "pmc_observability_node_metrics"} {
		if strings.Contains(descs[read], "WRITE:") || strings.Contains(descs[read], "DANGEROUS:") {
			t.Errorf("%s is a read tool but carries a write marker: %s", read, descs[read])
		}
	}
}
