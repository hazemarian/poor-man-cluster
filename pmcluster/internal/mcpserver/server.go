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
	// Allow is the highest tool tier served. Zero value is ReadOnly.
	Allow Tier
	// Version is stamped into the MCP implementation name.
	Version string
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
			Description: tool.Description,
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

// runTool executes one tool call, applying the tier gate first.
func runTool(ctx context.Context, base http.Handler, authz string, opts Options, t Tool, args map[string]any, catalog []Tool) (*mcp.CallToolResult, error) {
	if t.Tier > opts.Allow {
		return manualAction(tierRefusal(t, opts)), nil
	}
	if t.Method == "" {
		return runCurated(ctx, base, authz, t, args, catalog)
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
	Status      string `json:"status"`
	Tool        string `json:"tool"`
	Reason      string `json:"reason"`
	Command     string `json:"command,omitempty"`
	Where       string `json:"where,omitempty"`
	TellTheUser string `json:"tell_the_user"`
	DoNotRetry  bool   `json:"do_not_retry"`
}

func manualAction(p manualActionPayload) *mcp.CallToolResult {
	p.Status = "manual_action_required"
	p.DoNotRetry = true
	raw, _ := json.MarshalIndent(p, "", "  ")
	return textResult(string(raw))
}

// tierRefusal is the answer for a tool above the enabled tier.
func tierRefusal(t Tool, opts Options) manualActionPayload {
	reason := fmt.Sprintf("%s is a %s operation and this MCP endpoint serves up to %s", t.Name, t.Tier, opts.Allow)
	return manualActionPayload{
		Tool:        t.Name,
		Reason:      reason,
		Command:     "pmcluster " + cliHint(t),
		Where:       "on a cluster node with the data directory, or over the API with a token that allows the change",
		TellTheUser: "I cannot make this change: this MCP endpoint is read-only for it. Ask the user to run the command below themselves, or to raise the MCP tier.",
	}
}

// cliHint maps a REST operation back to the CLI command an operator would type.
func cliHint(t Tool) string {
	human := strings.TrimPrefix(t.Name, "pmc_")
	return strings.ReplaceAll(human, "_", " ")
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
