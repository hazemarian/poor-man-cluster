package auth

import (
	"context"
	"net/http"
	"strings"
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
}

type ctxKey struct{}

// FromContext returns the authenticated user, or nil if none is attached.
func FromContext(ctx context.Context) *User {
	u, _ := ctx.Value(ctxKey{}).(*User)
	return u
}

// Lookup is the contract Bearer needs from a user store. Implementations
// iterate users and call VerifyToken; argon2id salts are per-row so we
// can't look up by token directly.
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
