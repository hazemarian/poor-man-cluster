package mcpserver

// JSON Schema preparation: MCP clients receive each tool's inputSchema as a
// standalone document, so `$ref`s into `#/components/...` must be inlined (a
// client has no way to fetch the rest of the file) and OpenAPI-only keywords
// dropped (3.0 `nullable`, `example`, …) so the result is plain JSON Schema.

import (
	"fmt"
	"strings"
)

// openapiOnlyKeywords are removed from a copied schema. `format` is kept: it is
// valid JSON Schema and useful to clients.
var openapiOnlyKeywords = map[string]bool{
	"nullable":              true,
	"example":               true,
	"examples":              true,
	"discriminator":         true,
	"xml":                   true,
	"externalDocs":          true,
	"deprecated":            true,
	"readOnly":              true,
	"writeOnly":             true,
	"$ref":                  true, // handled by resolve, never copied
	"operationId":           true,
	"x-pmcluster-extension": true,
}

// clean copies a schema node, resolving $ref and merging `nullable: true` into an
// optional null type so the result survives strict clients.
func clean(node any, spec map[string]any) (any, error) {
	return cleanDepth(node, spec, 0)
}

func cleanDepth(node any, spec map[string]any, depth int) (any, error) {
	if depth > 12 {
		return map[string]any{}, nil // a cycle guard; the specs here are shallow
	}
	switch v := node.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			target, err := resolve(spec, ref)
			if err != nil {
				return nil, err
			}
			merged, err := cleanDepth(target, spec, depth+1)
			if err != nil {
				return nil, err
			}
			// Sibling keys override the referenced schema (OpenAPI 3.0 ignores
			// them, 3.1 merges them; merging is the least surprising).
			if len(v) > 1 {
				base, _ := merged.(map[string]any)
				for k, val := range v {
					if k == "$ref" {
						continue
					}
					base[k] = val
				}
			}
			return merged, nil
		}
		out := make(map[string]any, len(v))
		var nullable bool
		for k, val := range v {
			if openapiOnlyKeywords[k] {
				if k == "nullable" {
					nullable, _ = val.(bool)
				}
				continue
			}
			cleaned, err := cleanDepth(val, spec, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = cleaned
		}
		if nullable {
			out["type"] = appendNullable(out["type"])
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			cleaned, err := cleanDepth(item, spec, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, cleaned)
		}
		return out, nil
	default:
		return node, nil
	}
}

// appendNullable turns "string" into ["string", "null"].
func appendNullable(t any) any {
	switch v := t.(type) {
	case string:
		if v == "" {
			return t
		}
		return []any{v, "null"}
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && s == "null" {
				return v
			}
		}
		return append(v, "null")
	}
	return t
}

// resolve walks a local JSON pointer (#/a/b/c).
func resolve(spec map[string]any, ref string) (any, error) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("external $ref %q cannot be inlined", ref)
	}
	var cur any = spec
	for _, seg := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("$ref %q: segment %q is not an object", ref, seg)
		}
		next, ok := m[seg]
		if !ok {
			return nil, fmt.Errorf("$ref %q does not exist", ref)
		}
		cur = next
	}
	return cur, nil
}
