// Package mcpserver serves the Model Context Protocol surface of the pmcluster
// daemon as a sub-route of its own API: same router, same handlers, same Bearer
// token, no second implementation of anything.
package mcpserver

// Catalog derivation: every operation in the checked-in OpenAPI document
// becomes one MCP tool. The document is load-bearing — internal/docscheck fails
// the build when it disagrees with the handlers — so a tool cannot describe an
// endpoint that does not exist, and an endpoint cannot appear without a tool.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Tier is what a tool is allowed to do. The daemon serves reads by default and
// requires an explicit opt-in for each step up.
type Tier int

const (
	// ReadOnly tools only GET.
	ReadOnly Tier = iota
	// Write tools change cluster state but cannot destroy data.
	Write
	// Dangerous tools run commands in containers, reveal secret values, delete
	// stacks, or rotate TLS material.
	Dangerous
)

func (t Tier) String() string {
	switch t {
	case Write:
		return "write"
	case Dangerous:
		return "dangerous"
	default:
		return "read"
	}
}

// Tool is one MCP tool: a documented operation, or a curated composite.
type Tool struct {
	Name        string
	Description string
	Method      string // HTTP method for derived tools; "" for curated ones
	Path        string // path template as it appears in the spec
	PathParams  []string
	QueryParams []string
	Tier        Tier
	InputSchema json.RawMessage
}

// excludedOps are documented operations that must not become tools, each for a
// reason that is about meaning rather than convenience.
var excludedOps = map[string]string{
	"/health":                                 "liveness probe; the MCP endpoint answering at all is the health signal",
	"/webhook/{source}":                       "HMAC CI ingress, not an operator action — an agent must not forge a deploy",
	"/api/public/badge/{stack}":               "SVG image for READMEs",
	"/api/public/badge/{stack}/services":      "SVG image for READMEs",
	"/api/public/badge/{stack}/{service}":     "SVG image for READMEs",
	"/api/services/{stack}/{service}/exec/ws": "interactive websocket; the JSON exec operation covers programmatic use",
	"/api/mcp": "the MCP endpoint itself: a tool that speaks JSON-RPC to itself is a loop",
}

// dangerousOps is the small set of documented writes that a misled model must
// not be able to run without a deliberate opt-in.
var dangerousOps = map[string]bool{
	"POST /api/services/{stack}/{service}/exec": true,
	"GET /api/secrets/{name}/value":             true,
	"DELETE /api/api_keys/{id}":                 true,
	"PUT /api/tls/site":                         true,
	"DELETE /api/tls/hosts/{host}":              true,
	"DELETE /api/stacks/{name}":                 true,
	"POST /api/backups/{id}/restore":            true,
}

// BuildCatalog parses the OpenAPI document into the derived tool set, sorted by
// name so the catalog is stable across builds.
func BuildCatalog(spec []byte) ([]Tool, error) {
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		return nil, fmt.Errorf("parse openapi: %w", err)
	}
	// The same document again as a generic tree: $ref resolution needs to walk
	// it by pointer.
	var full map[string]any
	if err := yaml.Unmarshal(spec, &full); err != nil {
		return nil, fmt.Errorf("parse openapi: %w", err)
	}
	if len(doc.Paths) == 0 {
		return nil, fmt.Errorf("parse openapi: no paths")
	}

	var tools []Tool
	for path, ops := range doc.Paths {
		if _, skip := excludedOps[path]; skip {
			continue
		}
		for method, raw := range ops {
			up := strings.ToUpper(method)
			switch up {
			case "GET", "POST", "PUT", "PATCH", "DELETE":
			default:
				continue
			}
			var op operation
			if err := yaml.Unmarshal(raw, &op); err != nil {
				return nil, fmt.Errorf("parse %s %s: %w", up, path, err)
			}
			if op.OperationID == "" {
				return nil, fmt.Errorf("%s %s has no operationId — every operation needs one to become a tool", up, path)
			}
			inlined, err := op.inputSchema(full)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", up, path, err)
			}
			tier := Write
			if up == "GET" {
				tier = ReadOnly
			}
			if dangerousOps[up+" "+path] {
				tier = Dangerous
			}
			tools = append(tools, Tool{
				Name:        "pmc_" + snake(op.OperationID),
				Description: describe(up, path, op),
				Method:      up,
				Path:        path,
				PathParams:  pathParams(path),
				QueryParams: op.queryNames(),
				Tier:        tier,
				InputSchema: inlined,
			})
		}
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, nil
}

type operation struct {
	OperationID string `json:"operationId"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Parameters  []struct {
		Ref      string `json:"$ref"`
		Name     string `json:"name"`
		In       string `json:"in"`
		Required bool   `json:"required"`
		Schema   any    `json:"schema"`
	} `json:"parameters"`
	RequestBody *struct {
		Required bool `json:"required"`
		Content  map[string]struct {
			Schema any `json:"schema"`
		} `json:"content"`
	} `json:"requestBody"`
}

func describe(method, path string, op operation) string {
	var b strings.Builder
	if op.Summary != "" {
		b.WriteString(op.Summary)
	} else {
		b.WriteString(method + " " + path)
	}
	b.WriteString("\n\nHTTP: " + method + " " + path)
	if d := strings.TrimSpace(op.Description); d != "" {
		b.WriteString("\n" + d)
	}
	return b.String()
}

// inputSchema merges path parameters, query parameters and the JSON request
// body into the single flat object MCP requires, with $refs inlined.
func (o operation) inputSchema(spec map[string]any) (json.RawMessage, error) {
	props := map[string]any{}
	var required []string
	for _, p := range o.Parameters {
		if p.In == "header" {
			continue // signature headers belong to the webhook path, which is excluded
		}
		name := p.Name
		schema := p.Schema
		if p.Ref != "" {
			resolved, err := resolve(spec, p.Ref)
			if err != nil {
				return nil, err
			}
			m, _ := resolved.(map[string]any)
			if m == nil {
				continue
			}
			name, _ = m["name"].(string)
			schema = m["schema"]
			if req, _ := m["required"].(bool); req {
				p.Required = true
			}
		}
		if name == "" {
			continue
		}
		cleaned, err := clean(schema, spec)
		if err != nil {
			return nil, fmt.Errorf("parameter %s: %w", name, err)
		}
		props[name] = cleaned
		if p.Required {
			required = append(required, name)
		}
	}
	if o.RequestBody != nil {
		if media, ok := o.RequestBody.Content["application/json"]; ok {
			cleaned, err := clean(media.Schema, spec)
			if err != nil {
				return nil, fmt.Errorf("request body: %w", err)
			}
			m, _ := cleaned.(map[string]any)
			bodyProps, _ := m["properties"].(map[string]any)
			for k, v := range bodyProps {
				props[k] = v
			}
			if req, _ := m["required"].([]any); len(req) > 0 {
				for _, r := range req {
					if s, ok := r.(string); ok {
						required = append(required, s)
					}
				}
			} else if list, ok := m["required"].([]string); ok {
				required = append(required, list...)
			}
		}
	}
	sort.Strings(required)
	schema := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = dedupeStrings(required)
	}
	out, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// pathParams returns the {param} names in a path template, in order.
func pathParams(path string) []string {
	var out []string
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, strings.Trim(seg, "{}"))
		}
	}
	return out
}

func (o operation) queryNames() []string {
	var out []string
	for _, p := range o.Parameters {
		if p.In == "query" && p.Name != "" {
			out = append(out, p.Name)
		}
	}
	return out
}

// snake converts an operationId to a tool suffix: listStacks → list_stacks,
// listAPIKeys → list_api_keys (a run of capitals is one word, not three
// single-letter ones), putSiteCert → put_site_cert.
func snake(s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		switch {
		case r == '-' || r == ' ' || r == '/':
			b.WriteByte('_')
		case r >= 'A' && r <= 'Z':
			prevLower := i > 0 && runes[i-1] >= 'a' && runes[i-1] <= 'z'
			nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
			if i > 0 && (prevLower || (nextLower && runes[i-1] >= 'A' && runes[i-1] <= 'Z')) {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
