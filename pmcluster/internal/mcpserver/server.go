package mcpserver

// The MCP surface is a sub-route of the daemon API (/api/mcp), not a second
// service. Every tool call re-enters the daemon's own router in-process with the
// caller's Authorization header, so auth.Bearer, stackScopeGuard, RequireRole and
// the rate limiter all run exactly as they do for a REST call: same handlers,
// same token, no parallel implementation to keep in sync.
//
// Three behaviours are deliberate:
//
//   - Reads are served by default; writes and dangerous operations require an
//     explicit opt-in tier (Options.Allow), because an LLM holding a write token
//     against a production control plane is not a default.
//   - A gated or impossible action does not return a bare error. It returns a
//     structured "manual_action_required" result naming the command and where to
//     run it, so the agent can tell the operator what to do instead of guessing
//     or silently failing.
//   - Nothing is invented: a daemon error is passed through verbatim.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Options configures the MCP surface.
type Options struct {
	// Spec is pmcluster/docs/openapi.yaml, embedded.
	Spec []byte
	// AllowOps is the set of write operations ("METHOD /path") an agent may
	// call. Reads are always served; a write that is not in this set is refused
	// with instructions instead. Nil means DefaultAllowedOps — never "all".
	AllowOps map[string]bool
	// Version is stamped into the MCP implementation name.
	Version string
}

// DefaultAllowedOps is the out-of-the-box write surface: update the content of a
// service-scoped config, and nothing else. Every other write (deploy, restart,
// delete, settings, exec, secret reveal) is answered with a manual-action
// refusal, because an agent editing production infrastructure unattended is not
// a default.
var DefaultAllowedOps = map[string]bool{
	"PUT /api/configs/{name}": true,
}

// AllWrites is every non-dangerous write. Still never includes the dangerous
// tier: exec, secret reveal, stack delete, TLS rotation and backup restore stay
// manual whatever the operator sets.
func AllWrites(spec []byte) (map[string]bool, error) {
	catalog, err := BuildCatalog(spec)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, t := range catalog {
		if t.Tier == Write {
			out[t.Method+" "+t.Path] = true
		}
	}
	return out, nil
}

// Handler builds the streamable-HTTP MCP handler. base must be the daemon's
// router; tool calls are dispatched through it.
func Handler(base http.Handler, opts Options) (http.Handler, error) {
	derived, err := BuildCatalog(opts.Spec)
	if err != nil {
		return nil, err
	}
	all := make([]Tool, 0, len(derived)+len(curatedTools()))
	all = append(all, derived...)
	all = append(all, curatedTools()...)
	for _, t := range all {
		if !json.Valid(t.InputSchema) {
			return nil, fmt.Errorf("tool %s has an invalid input schema", t.Name)
		}
	}
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return buildServer(base, opts, all, r)
	}, nil), nil
}

// buildServer registers the catalog for one HTTP request. The request carries the
// bearer token every tool call is dispatched with.
func buildServer(base http.Handler, opts Options, tools []Tool, r *http.Request) *mcp.Server {
	name := "pmcluster"
	if opts.Version != "" {
		name = "pmcluster " + opts.Version
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: opts.Version}, nil)
	authz := r.Header.Get("Authorization")
	for _, t := range tools {
		tool := t
		srv.AddTool(&mcp.Tool{
			Name:        tool.Name,
			Description: describeWithPolicy(opts, tool),
			InputSchema: tool.InputSchema,
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := map[string]any{}
			if len(req.Params.Arguments) > 0 {
				_ = json.Unmarshal(req.Params.Arguments, &args)
			}
			return runTool(ctx, base, authz, opts, tool, args, tools)
		})
	}
	return srv
}

// describeWithPolicy appends this endpoint's permission for a write tool to its
// description. Without it the model learns the limit only by calling the tool and
// reading the refusal — and a tool list that advertises `Create a webhook` on an
// endpoint that refuses writes is misleading in exactly the direction that wastes
// a turn. Reads carry no marker: they are all served.
func describeWithPolicy(opts Options, t Tool) string {
	switch {
	case t.Tier == ReadOnly:
		return t.Description
	case t.Tier == Dangerous:
		return t.Description + "\n\nDANGEROUS: never served through MCP. A call answers manual_action_required " +
			"naming the command a human must run. Do not attempt it, and do not look for a workaround."
	case allowed(opts, t):
		return t.Description + "\n\nWRITE: permitted on this MCP endpoint."
	default:
		return t.Description + "\n\nWRITE: NOT permitted on this MCP endpoint. A call answers " +
			"manual_action_required with the command a human must run — relay it, do not retry."
	}
}

// runTool executes one tool call, applying the tier gate first.
func runTool(ctx context.Context, base http.Handler, authz string, opts Options, t Tool, args map[string]any, catalog []Tool) (*mcp.CallToolResult, error) {
	if t.Tier != ReadOnly && !allowed(opts, t) {
		return manualAction(refusal(t, opts)), nil
	}
	if t.Method == "" {
		return runCurated(ctx, base, authz, t, args, catalog, opts)
	}
	// The one permitted write is "update a service-scoped config". Cluster-scope
	// configs are the platform's own templates and secrets-adjacent material, so
	// the tool checks the target's scope before changing it.
	if t.Method+" "+t.Path == "PUT /api/configs/{name}" {
		name, _ := args["name"].(string)
		cfg, err := get(ctx, base, authz, "/api/configs/"+name)
		if err != nil {
			return errorResult(fmt.Sprintf("cannot read config %q before updating it: %v", name, err)), nil
		}
		if scope, _ := cfg["scope"].(string); scope != "service" {
			return manualAction(manualActionPayload{
				Tool:        t.Name,
				Reason:      fmt.Sprintf("config %q has scope %q; only service-scoped configs may be changed through MCP", name, scope),
				Command:     fmt.Sprintf("pmcluster config set %s --content-file <file>", name),
				Where:       "on a cluster node",
				TellTheUser: fmt.Sprintf("I did not change %q: it is a %s-scope config. Ask the user to update it themselves.", name, scope),
			}), nil
		}
	}
	status, body, err := call(ctx, base, authz, t, args)
	if err != nil {
		return errorResult(fmt.Sprintf("%s failed: %v", t.Name, err)), nil
	}
	if status < 200 || status >= 300 {
		// Honest failure: the daemon's own words, with the status code.
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("HTTP %d: %s", status, strings.TrimSpace(string(body)))}},
		}, nil
	}
	out := strings.TrimSpace(string(body))
	if out == "" {
		out = fmt.Sprintf(`{"status":"ok","http":%d}`, status)
	}
	return textResult(out), nil
}

// call builds and dispatches the REST request for one derived tool.
func call(ctx context.Context, base http.Handler, authz string, t Tool, args map[string]any) (int, []byte, error) {
	path := t.Path
	for _, p := range t.PathParams {
		v, ok := args[p]
		if !ok {
			return 0, nil, fmt.Errorf("missing required argument %q", p)
		}
		path = strings.ReplaceAll(path, "{"+p+"}", fmt.Sprint(v))
	}
	query := make([]string, 0, len(t.QueryParams))
	for _, q := range t.QueryParams {
		if v, ok := args[q]; ok && v != nil && v != "" {
			query = append(query, q+"="+urlEncode(fmt.Sprint(v)))
		}
	}
	target := path
	if len(query) > 0 {
		target += "?" + strings.Join(query, "&")
	}

	var body io.Reader
	if t.Method != http.MethodGet && t.Method != http.MethodDelete {
		payload := map[string]any{}
		for k, v := range args {
			if contains(t.PathParams, k) || contains(t.QueryParams, k) {
				continue
			}
			payload[k] = v
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, t.Method, target, body)
	if err != nil {
		return 0, nil, err
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// In-process dispatch through the daemon's own router: no network hop, no
	// second auth path. A real ResponseWriter is not available inside a tool
	// call, so the response is captured.
	rec := httptest.NewRecorder()
	base.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes(), nil
}

// get is the read helper used by the curated tools.
func get(ctx context.Context, base http.Handler, authz, path string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	base.ServeHTTP(rec, req)
	if rec.Code < 200 || rec.Code >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return out, nil
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

// manualAction renders the result an agent must relay to a human verbatim: what
// it could not do, why, the command, and where to run it.
type manualActionPayload struct {
	Status string `json:"status"`
	Tool   string `json:"tool"`
	Reason string `json:"reason"`
	// Command is the CLI invocation, present only when one exists and was
	// verified against the command tree. An agent relays this verbatim, so an
	// invented command is worse than no command.
	Command string `json:"command,omitempty"`
	// API is the exact REST call for this operation, derived from the spec. It
	// is always correct, so it is always present.
	API         string `json:"api"`
	Where       string `json:"where,omitempty"`
	TellTheUser string `json:"tell_the_user"`
	DoNotRetry  bool   `json:"do_not_retry"`
}

// operatorCommands maps an operation to the CLI that performs it. Only entries
// whose invocation is certain are listed — a test walks internal/cli and fails
// if a first token here is not a real pmcluster command. Everything else is
// answered with the REST recipe alone.
var operatorCommands = map[string]string{
	"POST /api/stacks":                             "pmcluster deploy <manifest>",
	"POST /api/stacks/{name}/rollback":             "pmcluster rollback <stack>",
	"POST /api/webhooks":                           "pmcluster webhook add <source>",
	"POST /api/api_keys":                           "pmcluster user create <name>",
	"PUT /api/configs/{name}":                      "pmcluster config set <name> <value>",
	"GET /api/secrets/{name}/value":                "pmcluster secret show <name>",
	"PUT /api/tls/site":                            "pmcluster tls site set --cert <pem> --key <pem>",
	"POST /api/update":                             "pmcluster cluster update",
	"POST /api/services/{stack}/{service}/restart": "pmcluster service restart <stack> <service>",
	"POST /api/backups":                            "pmcluster backup",
}

// operatorCommand returns the CLI (possibly empty) and the REST recipe for a
// tool. The recipe names the daemon path so a human or a script can run it
// without guessing which endpoint the refusal was about.
func operatorCommand(t Tool) (command, api string) {
	command = operatorCommands[t.Method+" "+t.Path]
	api = fmt.Sprintf("curl -sS -X %s https://<daemon>/api%s -H \"Authorization: Bearer $PMC_TOKEN\"", t.Method, t.Path)
	if t.Method != http.MethodGet && t.Method != http.MethodDelete {
		api += " -H 'Content-Type: application/json' -d @body.json"
	}
	return command, api
}

func manualAction(p manualActionPayload) *mcp.CallToolResult {
	p.Status = "manual_action_required"
	p.DoNotRetry = true
	raw, _ := json.MarshalIndent(p, "", "  ")
	return textResult(string(raw))
}

// allowed reports whether a write tool is on the operator's allowlist.
func allowed(opts Options, t Tool) bool {
	if t.Tier == Dangerous {
		return false // never, whatever the allowlist says
	}
	set := opts.AllowOps
	if set == nil {
		set = DefaultAllowedOps
	}
	return set[t.Method+" "+t.Path]
}

// refusal is the answer for a write the operator has not allowed.
func refusal(t Tool, opts Options) manualActionPayload {
	var reason string
	switch {
	case t.Tier == Dangerous:
		reason = fmt.Sprintf("%s is a dangerous operation (exec, secret reveal, deletion or TLS change) and is never served through MCP", t.Name)
	case len(opts.AllowOps) == 0:
		reason = fmt.Sprintf("%s is a write operation and no writes are enabled on this MCP endpoint", t.Name)
	default:
		reason = fmt.Sprintf("%s is a write operation that is not on this MCP endpoint's allowlist; only config content updates are enabled", t.Name)
	}
	command, api := operatorCommand(t)
	how := "the REST call below"
	if command != "" {
		how = "the command below (or its REST equivalent)"
	}
	return manualActionPayload{
		Tool:        t.Name,
		Reason:      reason,
		Command:     command,
		API:         api,
		Where:       "on a cluster node with the data directory or a leader's shell, using a token that allows the change",
		TellTheUser: "I cannot make this change myself: it is not permitted on this MCP endpoint. Ask the user to run " + how + ".",
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// urlEncode escapes a query value without pulling net/url into the hot path.
func urlEncode(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == '~', r == ':':
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, "%%%02X", r)
		}
	}
	return b.String()
}

// ToolsByTier groups the catalog for the capability tool and for tests.
func ToolsByTier(tools []Tool) map[string][]string {
	out := map[string][]string{}
	for _, t := range tools {
		out[t.Tier.String()] = append(out[t.Tier.String()], t.Name)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}
