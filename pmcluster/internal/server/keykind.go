package server

import (
	"net/http"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/apikeys"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
)

// refuseMCPKeys closes the REST control plane to keys minted for MCP.
//
// An "mcp" key exists so an AI client can call tools — deploy, read logs, sync,
// restart — without holding a credential that also reads settings, secrets and
// rendered configs. The MCP endpoint already exposes exactly those tools with
// their own confirmation story, so the right shape is: this key is REFUSED on
// every other route, rather than relying on each handler to remember.
//
// It runs inside the /api group, after auth.Bearer, which means /api/mcp itself
// (mounted outside the group with its own bearer chain) is unaffected.
func refuseMCPKeys(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := auth.FromContext(r.Context()); u != nil && u.Kind == apikeys.KindMCP {
			writeErr(w, http.StatusForbidden,
				"this key is MCP-only: it may call /api/mcp and nothing else")
			return
		}
		next.ServeHTTP(w, r)
	})
}
