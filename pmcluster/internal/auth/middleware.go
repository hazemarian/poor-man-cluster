package auth

import (
	"context"
	"net/http"
	"strings"
)

// Daemon role tiers. The daemon's own tokens are much coarser than the UI's
// console roles: the bootstrap admin + the edge console token are "admin"
// (they may read secret material), freshly-minted API keys are "operator",
// and "viewer" exists so a read-only bearer can be denied secret surfaces.
// Hierarchical: admin > operator > viewer.
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// User is the authenticated principal attached to the request context by
// Bearer middleware.
type User struct {
	ID   int64
	Name string
	// Stack optionally scopes this token to a single application stack
	// (from users.stack). Empty means unscoped: the token may operate on
	// every stack, exactly as before per-stack scoping existed.
	Stack string
	// Role is the daemon-side tier (admin/operator/viewer) used to gate
	// secret-bearing surfaces (rendered configs, settings). Empty is treated
	// as "viewer" (least privilege) — see RoleAtLeast.
	Role string
}

type ctxKey struct{}

// FromContext returns the authenticated user, or nil if none is attached.
func FromContext(ctx context.Context) *User {
	u, _ := ctx.Value(ctxKey{}).(*User)
	return u
}

// roleRank returns the numeric precedence of a daemon role. Unknown/empty
// roles rank as viewer (least privilege) so a missing role never escalates.
func roleRank(role string) int {
	switch role {
	case RoleAdmin:
		return 3
	case RoleOperator:
		return 2
	case RoleViewer:
		return 1
	}
	return 1
}

// RoleAtLeast reports whether u holds at least the given role.
func RoleAtLeast(u *User, min string) bool {
	if u == nil {
		return false
	}
	return roleRank(u.Role) >= roleRank(min)
}

// IsAdmin reports whether u is an admin (may read secret material).
func IsAdmin(u *User) bool {
	return u != nil && u.Role == RoleAdmin
}

// RequireRole is chi middleware allowing only an authenticated user holding at
// least the given role. It must run after Bearer (the user is read from
// context). Insufficient or missing role yields 403.
func RequireRole(min string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !RoleAtLeast(FromContext(r.Context()), min) {
				http.Error(w, "forbidden: insufficient role", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Lookup is the contract Bearer needs from a user store. Implementations
// iterate users and call VerifyToken; argon2id salts are per-row so a
// direct lookup by token is impossible.
type Lookup interface {
	UserByToken(ctx context.Context, token string) (*User, error)
}

// Bearer requires `Authorization: Bearer <token>` and looks it up via
// lookup. Every failure mode returns the same 401 to avoid leaking which
// case it was.
func Bearer(lookup Lookup) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := extractBearer(r.Header.Get("Authorization"))
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="pmcluster"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			user, err := lookup.UserByToken(r.Context(), token)
			if err != nil || user == nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="pmcluster"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), ctxKey{}, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractBearer parses "Authorization: Bearer <token>" — case-insensitive
// scheme, tolerant of trailing whitespace, rejects empty/other schemes.
func extractBearer(header string) (string, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", false
	}
	const scheme = "bearer "
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(header[len(scheme):])
	if token == "" {
		return "", false
	}
	return token, true
}
