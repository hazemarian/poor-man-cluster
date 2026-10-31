// Package docs embeds the API description so the daemon can serve an MCP tool
// catalog derived from the same document the REST API is checked against.
//
// The spec is not a second source of truth here: internal/docscheck fails the
// build when openapi.yaml and the handlers disagree, so deriving tools from it
// keeps handlers → spec → tools in one chain.
package docs

import _ "embed"

// OpenAPI is pmcluster/docs/openapi.yaml, embedded at build time.
//
//go:embed openapi.yaml
var OpenAPI []byte
