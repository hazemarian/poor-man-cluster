package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
)

// ErrUserExists is returned by CreateUser when the name is already taken.
var ErrUserExists = errors.New("user already exists")

// ErrUserNotFound is returned by DeleteUser/UserByID when no row matches.
var ErrUserNotFound = errors.New("user not found")

// CreateUser stores a user with a v2-format token.  tokenID is the hex
// public-index part (from auth.SplitToken); tokenHash is the argon2id
// hash of the secret.
//
// stack optionally scopes the token to ONE application stack: pass a single
// non-empty value ("demo") to mint a stack-scoped token, or omit it (or pass
// "") for the default unscoped token that may operate on every stack. Only
// the first non-empty value is used; extra values are ignored.
//
// For legacy users (tokenID empty) the row is inserted with a NULL
// token_id and will fall back to the O(N) scan path in UserByToken.
func (s *Store) CreateUser(ctx context.Context, name, tokenID, tokenHash string, stack ...string) (int64, error) {
	// The bootstrap admin and the edge console token are minted here and keep
	// the admin role; freshly-minted API keys call CreateUserWithRole with
	// auth.RoleOperator instead (FIX 5).
	return s.CreateUserWithRole(ctx, name, tokenID, tokenHash, auth.RoleAdmin, stack...)
}

// CreateUserWithRole is CreateUser with an explicit daemon role tier (FIX 5).
func (s *Store) CreateUserWithRole(ctx context.Context, name, tokenID, tokenHash, role string, stack ...string) (int64, error) {
	if role == "" {
		role = auth.RoleAdmin
	}
	var tid sql.NullString
	if tokenID != "" {
		tid = sql.NullString{String: tokenID, Valid: true}
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (name, token_id, token_hash, created_at, stack, role) VALUES (?, ?, ?, ?, ?, ?)`,
		name, tid, tokenHash, time.Now().Unix(), firstStack(stack), role,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrUserExists
		}
		return 0, fmt.Errorf("insert user: %w", err)
	}
	return res.LastInsertId()
}

// firstStack resolves the optional variadic stack scope: the first
// non-empty value wins, "" when no scope was supplied.
func firstStack(stack []string) string {
	for _, s := range stack {
		if s != "" {
			return s
		}
	}
	return ""
}

// UserRow is a lightweight non-secret user record for listing API keys in
// the operator UI. It deliberately exposes no token material.
type UserRow struct {
	ID         int64
	Name       string
	CreatedAt  int64
	LastUsedAt int64
	// Stack is the optional per-token stack scope ("" = unscoped).
	Stack string
}

// ListUsers returns every user (id, name, created_at, last_used_at, stack)
// ordered by name, without any token/hash material.
func (s *Store) ListUsers(ctx context.Context) ([]UserRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, created_at, last_used_at, stack FROM users ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("query users: %w", err)
	}
	defer rows.Close()
	var out []UserRow
	for rows.Next() {
		var u UserRow
		if err := rows.Scan(&u.ID, &u.Name, &u.CreatedAt, &u.LastUsedAt, &u.Stack); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// minTokenLength is the shortest plausible pmcluster bearer token: a legacy
// `pmc_<secret>` token is ~47 chars and a v2 `pmc_<id>_<secret>` ~56 chars, so
// anything under 40 chars cannot be a real token (FIX 4).
const minTokenLength = 40

// UserByToken looks up a user by bearer token.
//
//   - v2 tokens ("pmc_<id>_<secret>"): the token_id is extracted and used
//     for a direct indexed lookup.  Argon2id runs exactly once against
//     the matched row.
//   - Legacy tokens ("pmc_<secret>", one segment, no id): falls back to the
//     old O(N) scan, iterating every user row.  This path exists only for
//     backwards compatibility; once all users have been re-issued v2 tokens,
//     the fallback can be removed.
//
// Any token that does not start with "pmc_" or is implausibly short is
// fast-rejected with (nil, nil) BEFORE any database work: the legacy scan
// runs argon2id (t=2, m=64MiB) against every user row, so letting arbitrary
// garbage through would let an attacker burn CPU per request (FIX 4).
func (s *Store) UserByToken(ctx context.Context, token string) (*auth.User, error) {
	if !strings.HasPrefix(token, "pmc_") || len(token) < minTokenLength {
		return nil, nil
	}
	tokenID, secret := auth.SplitToken(token)

	var (
		u   *auth.User
		err error
	)
	if tokenID != "" {
		u, err = s.userByTokenID(ctx, tokenID, secret)
	} else {
		u, err = s.userByTokenLegacy(ctx, token)
	}
	// Best-effort last-used tracking on every successful lookup (both the v2
	// and legacy paths). The touch must never fail auth, so its error is
	// deliberately ignored.
	if u != nil {
		_ = s.TouchUser(ctx, u.ID)
	}
	return u, err
}

// userByTokenID does a single-row lookup by the public token_id and
// verifies the argon2id hash against the secret. The row's stack scope (if
// any) is carried onto the returned user for the middleware to enforce.
func (s *Store) userByTokenID(ctx context.Context, tokenID, secret string) (*auth.User, error) {
	var (
		u    auth.User
		hash string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, stack, role, token_hash FROM users WHERE token_id = ?`, tokenID,
	).Scan(&u.ID, &u.Name, &u.Stack, &u.Role, &hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("query user by token_id: %w", err)
	}
	ok, err := auth.VerifyToken(secret, hash)
	if err != nil {

		return nil, nil
	}
	if !ok {
		return nil, nil
	}
	return &u, nil
}

// userByTokenLegacy is the old O(N) scan kept for backwards compatibility
// with pre-v2 tokens.  It iterates ALL user rows and runs argon2id against
// each until a match is found.
func (s *Store) userByTokenLegacy(ctx context.Context, token string) (*auth.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, stack, role, token_hash FROM users`)
	if err != nil {
		return nil, fmt.Errorf("query users: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			u    auth.User
			hash string
		)
		if err := rows.Scan(&u.ID, &u.Name, &u.Stack, &u.Role, &hash); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		ok, err := auth.VerifyToken(token, hash)
		if err != nil {

			continue
		}
		if ok {
			return &u, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	return nil, nil
}

// UserByID returns (nil, sql.ErrNoRows) when not found.
func (s *Store) UserByID(ctx context.Context, id int64) (*auth.User, error) {
	var u auth.User
	err := s.db.QueryRowContext(ctx, `SELECT id, name, stack FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Name, &u.Stack)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("query user: %w", err)
	}
	return &u, nil
}

// UserByName returns the non-secret row for a user looked up by name.
// Returns ErrUserNotFound when no row matches.
func (s *Store) UserByName(ctx context.Context, name string) (*UserRow, error) {
	var u UserRow
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, created_at, last_used_at, stack FROM users WHERE name = ?`, name).
		Scan(&u.ID, &u.Name, &u.CreatedAt, &u.LastUsedAt, &u.Stack)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("query user by name: %w", err)
	}
	return &u, nil
}

// TouchUser records a successful API-key use for a user, best-effort. The
// returned error is intentionally ignorable: last-used tracking must never
// break authentication.
func (s *Store) TouchUser(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE users SET last_used_at = ? WHERE id = ?`, time.Now().Unix(), id,
	); err != nil {
		return fmt.Errorf("touch user: %w", err)
	}
	return nil
}

// DeleteUser removes a user row (revoking its bearer token immediately).
// Returns ErrUserNotFound when no row matched.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// isUniqueViolation matches modernc.org/sqlite's UNIQUE error strings.
// Brittle; kept isolated here so a driver swap is a one-liner.
func isUniqueViolation(err error) bool {
	return err != nil && (containsAny(err.Error(),
		"UNIQUE constraint failed",
		"constraint failed: UNIQUE",
	))
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
