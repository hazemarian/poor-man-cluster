package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/manifest"
)

// maxDeployScanBytes bounds the deploy body the scope guard is willing to
// read to resolve the target stack. It matches the stacks deploy handler's
// own 1 MiB body limit; anything larger can never deploy anyway.
const maxDeployScanBytes = 1 << 20

// stackScopeGuard runs directly after auth.Bearer on the /api subtree and
// enforces per-stack API token scoping.
//
// A token minted with a stack scope (users.stack != ”) may only operate on
// that one stack: its deploy/sync/rollback/delete routes under /api/stacks
// and its list/tasks/logs/restart/exec routes under /api/services, and only
// when the request actually targets that stack. Every other /api route —
// settings, usage, api_keys, tls, webhooks, backups, cluster info, even
// /api/me — is refused with 403 {"error":"token scoped to stack <x>"}.
//
// Tokens without a stack scope (”, every pre-existing row and the 'edge'
// console user) fall straight through: behaviour is unchanged for them.
//
// Requests that name their stack in the URL (/api/stacks/{name}/…,
// /api/services/{stack}/…) are checked against the path. The one exception
// is deploy (POST /api/stacks), which names its stack in the JSON body, so
// the guard peeks at the payload (app_name, else the manifest's app name)
// — reading it without consuming it, so the handler still sees the body.
// When no stack can be resolved the request is refused (fail closed).
func stackScopeGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := auth.FromContext(r.Context())
		if u == nil || u.Stack == "" {
			// Unscoped token: full access, exactly as before.
			next.ServeHTTP(w, r)
			return
		}
		target, onStackSurface := stackTarget(r)
		if !onStackSurface || target != u.Stack {
			writeErr(w, http.StatusForbidden, "token scoped to stack "+u.Stack)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// stackTarget resolves which stack a request operates on.
//
// It returns (target, true) for requests on the stack surface — anything
// under /api/stacks and /api/services — and ("", false) for every other
// route, which a stack-scoped token may never reach anyway.
//
// target is the stack named by the first path segment, or, for a deploy
// (POST /api/stacks), the stack named by the deploy payload. target == ""
// means "no single stack resolved" (collection listings, unparseable
// bodies), which never matches a non-empty scope.
func stackTarget(r *http.Request) (string, bool) {
	for _, prefix := range []string{"/api/stacks", "/api/services"} {
		rest, ok := trimPathPrefix(r.URL.Path, prefix)
		if !ok {
			continue
		}
		if rest == "" {
			// Collection route: GET /api/stacks, GET /api/services, or a
			// deploy (POST /api/stacks). Only a deploy names a stack, and
			// it does so in its body.
			if prefix == "/api/stacks" && r.Method == http.MethodPost {
				return deployTargetStack(r), true
			}
			return "", true
		}
		// /api/stacks/{name}… and /api/services/{stack}… — the first
		// segment is the stack (sync/rollback/revisions/tasks/logs/…).
		name := rest[1:]
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[:i]
		}
		return name, true
	}
	return "", false
}

// trimPathPrefix strips prefix from path when the path is exactly prefix or
// continues at a segment boundary ("/api/stacks" matches "/api/stacks/x"
// but not "/api/stacksx").
func trimPathPrefix(path, prefix string) (string, bool) {
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rest := path[len(prefix):]
	if rest == "" {
		return "", true
	}
	if rest[0] != '/' {
		return "", false
	}
	return rest, true
}

// deployTargetStack peeks at a deploy payload to learn which stack it would
// create: app_name wins (it overrides the manifest, mirroring Deploy), else
// the manifest's own app name. The body is read once and put back so the
// deploy handler still gets it verbatim. Any failure — missing body, bad
// JSON, unparseable manifest — yields "" so the request fails closed for a
// scoped token (and is untouched for an unscoped one, which never gets here).
func deployTargetStack(r *http.Request) string {
	if r.Body == nil || r.Body == http.NoBody {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDeployScanBytes+1))
	if err != nil {
		return ""
	}
	// Hand the bytes back to the handler either way.
	r.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) > maxDeployScanBytes {
		return ""
	}

	var p struct {
		AppName  string `json:"app_name"`
		Manifest string `json:"manifest"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return ""
	}
	if p.AppName != "" {
		return p.AppName
	}
	if p.Manifest == "" {
		return ""
	}
	app, err := manifest.Parse([]byte(p.Manifest))
	if err != nil {
		return ""
	}
	return app.Name
}
