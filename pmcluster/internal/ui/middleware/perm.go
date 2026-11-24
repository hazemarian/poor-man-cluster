package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
)

// A permission is checked against the ROLE the account holds, so adding a role in
// the console is enough to give somebody a different reach — no code change, and
// no second place where "who may do this" is written down.
//
// The mapping below is the single place that decides which permission a console
// route needs. It is deliberately a table rather than a gate on each route: a
// permission that no route reads is a checkbox that lies, and a route that bypasses
// the table is a hole nobody can see. TestPermissionMapCoversEveryRoute keeps the
// table and the router in step.

// routeRule requires perm for requests whose path starts with Prefix (and, when
// Methods is non-empty, whose method is listed).
type routeRule struct {
	Prefix  string
	Methods []string
	// Contains narrows a rule to paths holding any of these fragments, so
	// /stacks/<name>/secrets can need a different permission from
	// /stacks/<name>/sync without enumerating every stack.
	Contains []string
	Perm     string
}

// routeRules is ordered: the first match wins, so put the specific ones first.
var routeRules = []routeRule{
	// Access management.
	{Prefix: "/roles", Perm: store.PermRoles},
	{Prefix: "/users", Perm: store.PermUsers},
	{Prefix: "/apikeys", Perm: store.PermKeys},

	// Platform configuration. Reads stay where they were (the console always let a
	// viewer look); the writes split by consequence — saving cluster settings is
	// admin work, rotating a certificate or adding a webhook is operator work.
	// Rendered configs embed credentials, so they need the permission that already
	// means "may handle secrets" — which is what the console enforced before roles
	// were data (viewers refused, operators allowed). The settings PAGE, which
	// lists names and shows forms, stays readable.
	{Prefix: "/settings/rendered", Perm: store.PermSecretsWrite},
	{Prefix: "/settings", Methods: []string{http.MethodPost}, Perm: store.PermSettingsWrite},
	{Prefix: "/settings", Perm: store.PermView},
	{Prefix: "/settings/configs", Methods: []string{http.MethodPost}, Perm: store.PermSettingsWrite},
	{Prefix: "/settings/secrets", Methods: []string{http.MethodPost}, Perm: store.PermSettingsWrite},
	{Prefix: "/tls", Methods: []string{http.MethodPost}, Perm: store.PermPlatformConfig},
	{Prefix: "/tls", Perm: store.PermView},
	{Prefix: "/webhooks", Methods: []string{http.MethodPost}, Perm: store.PermPlatformConfig},
	{Prefix: "/webhooks", Perm: store.PermView},

	// A shell runs commands inside a stack with its secrets mounted.
	{Prefix: "/shells", Perm: store.PermShells},
	{Prefix: "/terminal", Perm: store.PermShells},

	// Stack actions split into "change what runs" and "change what it is".
	// Secret/config writes come first: they carry credentials, so they are not
	// covered by the general operate permission.
	{Prefix: "/stacks/", Methods: []string{http.MethodPost}, Contains: []string{"/configs", "/secrets"}, Perm: store.PermSecretsWrite},
	{Prefix: "/stacks/", Methods: []string{http.MethodPost}, Perm: store.PermOperate},
	{Prefix: "/deploy", Methods: []string{http.MethodPost}, Perm: store.PermDeploy},

	// Backups: browsing is reading, taking/restoring is a mutation.
	{Prefix: "/backups", Methods: []string{http.MethodPost}, Perm: store.PermBackups},
}

// PermissionFor returns the permission a request needs.
//
// Defaults are the shape of the console before roles were data: reading needs
// console.view, anything that mutates needs stacks.operate. Routes that deserve
// more than that are named above.
func PermissionFor(method, path string) string {
	p := strings.TrimPrefix(path, WebBase)
	for _, r := range routeRules {
		if !strings.HasPrefix(p, r.Prefix) {
			continue
		}
		if len(r.Methods) > 0 {
			ok := false
			for _, m := range r.Methods {
				if method == m {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		if len(r.Contains) > 0 {
			found := false
			for _, frag := range r.Contains {
				if strings.Contains(p, frag) {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		return r.Perm
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		return store.PermView
	}
	return store.PermOperate
}

// Can is THE answer to "may this role do this". The route guard and the template
// helper both call it, so a page can never hide a control that the route would
// allow, or offer one it would refuse.
func Can(ctx context.Context, st *store.Store, roleName, perm string) bool {
	if roleName == "" || perm == "" {
		return false
	}
	// The admin role is a superset by definition: a console whose admin role was
	// edited down to nothing would have no way back in.
	if roleName == store.RoleAdmin {
		return true
	}
	role, err := st.RoleByName(ctx, roleName)
	if err != nil {
		return false // an account pointing at a deleted role has no permissions
	}
	return role.Has(perm)
}

// RequirePermission allows the request when the signed-in account's role grants
// the permission the route needs. It runs after Require, which is what puts the
// user (and therefore the role) in the context.
func (a *Auth) RequirePermission() gin.HandlerFunc {
	return func(c *gin.Context) {
		u := CurrentUser(c)
		if u == nil {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		need := PermissionFor(c.Request.Method, c.Request.URL.Path)
		// Same resolver the templates use (Can), so the page and the route agree.
		if !Can(c.Request.Context(), a.st, u.Role, need) {
			if c.GetHeader("HX-Request") != "" {
				c.Header("HX-Redirect", WebBase+"/")
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
			http.Error(c.Writer, "forbidden: this role does not grant "+need, http.StatusForbidden)
			c.Abort()
			return
		}
		c.Next()
	}
}

// HasPermission is Can bound to this auth's store, for callers that already hold
// an Auth.
func (a *Auth) HasPermission(ctx context.Context, roleName, perm string) bool {
	return Can(ctx, a.st, roleName, perm)
}
