package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
)

// openTestStore is a helper that opens a fresh in-temp-dir store and registers
// cleanup. Every test gets its own isolated DB.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// createV2User generates a v2 token and inserts the user, returning the
// full token string, the tokenID, and the secret.
func createV2User(t *testing.T, s *Store, name string) (token, tokenID, secret string) {
	t.Helper()
	var err error
	token, err = auth.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	tokenID, secret = auth.SplitToken(token)
	h, err := auth.HashToken(secret)
	if err != nil {
		t.Fatalf("HashToken: %v", err)
	}
	if _, err := s.CreateUser(context.Background(), name, tokenID, h); err != nil {
		t.Fatalf("CreateUser(%q): %v", name, err)
	}
	return
}

// TestCreateUser_HappyPath verifies that a new v2 user is inserted and
// returns a valid (non-zero) row ID.
func TestCreateUser_HappyPath(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	tok, tokenID, secret := "", "", ""
	var err error
	tok, err = auth.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	tokenID, secret = auth.SplitToken(tok)
	h, err := auth.HashToken(secret)
	if err != nil {
		t.Fatalf("HashToken: %v", err)
	}
	id, err := s.CreateUser(ctx, "alice", tokenID, h)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if id <= 0 {
		t.Errorf("CreateUser returned id=%d, want > 0", id)
	}
}

// TestCreateUser_UniqueCollision verifies that inserting a duplicate name
// returns ErrUserExists and not a raw DB error.
func TestCreateUser_UniqueCollision(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	createV2User(t, s, "alice")

	tok, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	tid, secret := auth.SplitToken(tok)
	h, _ := auth.HashToken(secret)
	_, err = s.CreateUser(ctx, "alice", tid, h)
	if !errors.Is(err, ErrUserExists) {
		t.Errorf("second CreateUser err = %v, want ErrUserExists", err)
	}
}

// TestCreateUser_UniqueTokenID verifies that two users can't share a token_id.
func TestCreateUser_UniqueTokenID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	tok, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	tid, secret := auth.SplitToken(tok)
	h, _ := auth.HashToken(secret)

	if _, err := s.CreateUser(ctx, "alice", tid, h); err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}

	h2, _ := auth.HashToken(secret)
	_, err = s.CreateUser(ctx, "bob", tid, h2)
	if err == nil {
		t.Error("expected UNIQUE violation on duplicate token_id, got nil")
	}
}

// TestCountUsers covers the empty-table and populated-table cases.
func TestCountUsers(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	n, err := s.CountUsers(ctx)
	if err != nil {
		t.Fatalf("CountUsers (empty): %v", err)
	}
	if n != 0 {
		t.Errorf("CountUsers on empty DB = %d, want 0", n)
	}

	createV2User(t, s, "alice")
	createV2User(t, s, "bob")

	n, err = s.CountUsers(ctx)
	if err != nil {
		t.Fatalf("CountUsers (populated): %v", err)
	}
	if n != 2 {
		t.Errorf("CountUsers after 2 inserts = %d, want 2", n)
	}
}

// TestUserByToken covers v2 token lookup.
func TestUserByToken(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	t.Run("unknown token returns nil", func(t *testing.T) {
		u, err := s.UserByToken(ctx, "pmc_deadbeef_nonexistent")
		if err != nil {
			t.Fatalf("UserByToken: %v", err)
		}
		if u != nil {
			t.Errorf("expected nil for unknown token, got %+v", u)
		}
	})

	tokAlice, _, _ := createV2User(t, s, "alice")
	tokBob, _, _ := createV2User(t, s, "bob")
	tokCarol, _, _ := createV2User(t, s, "carol")

	t.Run("valid token returns the right user", func(t *testing.T) {
		u, err := s.UserByToken(ctx, tokAlice)
		if err != nil {
			t.Fatalf("UserByToken: %v", err)
		}
		if u == nil {
			t.Fatal("expected user, got nil")
		}
		if u.Name != "alice" {
			t.Errorf("user.Name = %q, want %q", u.Name, "alice")
		}
	})

	t.Run("multi-user: each v2 token maps to its own user", func(t *testing.T) {
		for _, tok := range []string{tokAlice, tokBob, tokCarol} {
			u, err := s.UserByToken(ctx, tok)
			if err != nil {
				t.Fatalf("UserByToken(%q): %v", tok, err)
			}
			if u == nil {
				t.Fatalf("UserByToken(%q) = nil", tok)
			}
		}
	})

	t.Run("token for no user returns nil", func(t *testing.T) {
		u, err := s.UserByToken(ctx, "pmc_abcdef01_fake-secret")
		if err != nil {
			t.Fatalf("UserByToken: %v", err)
		}
		if u != nil {
			t.Errorf("expected nil for non-matching token, got %+v", u)
		}
	})
}

// TestUserByTokenLegacy verifies the legacy O(N) fallback still works for
// pre-v2 tokens.
func TestUserByTokenLegacy(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	legacyToken := "old-plain-legacy-token"
	h, err := auth.HashToken(legacyToken)
	if err != nil {
		t.Fatalf("HashToken: %v", err)
	}
	if _, err := s.CreateUser(ctx, "legacy-user", "", h); err != nil {
		t.Fatalf("CreateUser (legacy): %v", err)
	}

	u, err := s.UserByToken(ctx, legacyToken)
	if err != nil {
		t.Fatalf("UserByToken (legacy): %v", err)
	}
	if u == nil {
		t.Fatal("legacy token lookup returned nil, want user")
	}
	if u.Name != "legacy-user" {
		t.Errorf("legacy user.Name = %q, want 'legacy-user'", u.Name)
	}

	u, err = s.UserByToken(ctx, "nonexistent-legacy-token")
	if err != nil {
		t.Fatalf("UserByToken (legacy unknown): %v", err)
	}
	if u != nil {
		t.Errorf("expected nil for unknown legacy token, got %+v", u)
	}
}

// TestListUsers covers the empty table and a populated list ordered by name.
func TestListUsers(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	t.Run("empty returns no rows", func(t *testing.T) {
		rows, err := s.ListUsers(ctx)
		if err != nil {
			t.Fatalf("ListUsers (empty): %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("ListUsers on empty DB = %d rows, want 0", len(rows))
		}
	})

	createV2User(t, s, "charlie")
	createV2User(t, s, "alice")
	createV2User(t, s, "bob")

	t.Run("populated returns name-sorted rows", func(t *testing.T) {
		rows, err := s.ListUsers(ctx)
		if err != nil {
			t.Fatalf("ListUsers (populated): %v", err)
		}
		if len(rows) != 3 {
			t.Fatalf("ListUsers = %d rows, want 3", len(rows))
		}
		want := []string{"alice", "bob", "charlie"}
		for i, row := range rows {
			if row.Name != want[i] {
				t.Errorf("row[%d].Name = %q, want %q", i, row.Name, want[i])
			}
			if row.ID <= 0 {
				t.Errorf("row[%d].ID = %d, want > 0", i, row.ID)
			}
			if row.CreatedAt <= 0 {
				t.Errorf("row[%d].CreatedAt = %d, want > 0", i, row.CreatedAt)
			}
		}
	})
}

// TestUserByID covers the not-found (sql.ErrNoRows) path and the happy path.
func TestUserByID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	t.Run("unknown id returns sql.ErrNoRows", func(t *testing.T) {
		u, err := s.UserByID(ctx, 9999)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("UserByID(9999) err = %v, want sql.ErrNoRows", err)
		}
		if u != nil {
			t.Errorf("UserByID(9999) = %+v, want nil", u)
		}
	})

	t.Run("known id returns the right user", func(t *testing.T) {
		tok, err := auth.GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken: %v", err)
		}
		tid, secret := auth.SplitToken(tok)
		h, _ := auth.HashToken(secret)
		id, err := s.CreateUser(ctx, "dave", tid, h)
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		u, err := s.UserByID(ctx, id)
		if err != nil {
			t.Fatalf("UserByID(%d): %v", id, err)
		}
		if u == nil {
			t.Fatal("expected user, got nil")
		}
		if u.ID != id {
			t.Errorf("user.ID = %d, want %d", u.ID, id)
		}
		if u.Name != "dave" {
			t.Errorf("user.Name = %q, want dave", u.Name)
		}
	})
}

func TestUserByName(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	t.Run("unknown name returns ErrUserNotFound", func(t *testing.T) {
		_, err := s.UserByName(ctx, "nobody")
		if !errors.Is(err, ErrUserNotFound) {
			t.Errorf("UserByName(nobody) err = %v, want ErrUserNotFound", err)
		}
	})

	t.Run("known name returns the non-secret row", func(t *testing.T) {
		tok, _ := auth.GenerateToken()
		tid, secret := auth.SplitToken(tok)
		h, _ := auth.HashToken(secret)
		id, err := s.CreateUser(ctx, "eve", tid, h)
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		u, err := s.UserByName(ctx, "eve")
		if err != nil {
			t.Fatalf("UserByName: %v", err)
		}
		if u.ID != id || u.Name != "eve" {
			t.Errorf("UserByName = %+v, want id %d name eve", u, id)
		}
	})
}

func TestDeleteUser(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	t.Run("unknown id returns ErrUserNotFound", func(t *testing.T) {
		if err := s.DeleteUser(ctx, 9999); !errors.Is(err, ErrUserNotFound) {
			t.Errorf("DeleteUser(9999) err = %v, want ErrUserNotFound", err)
		}
	})

	t.Run("delete revokes the user row", func(t *testing.T) {
		tok, _ := auth.GenerateToken()
		tid, secret := auth.SplitToken(tok)
		h, _ := auth.HashToken(secret)
		id, err := s.CreateUser(ctx, "frank", tid, h)
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		if err := s.DeleteUser(ctx, id); err != nil {
			t.Fatalf("DeleteUser: %v", err)
		}
		if _, err := s.UserByID(ctx, id); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("UserByID after delete = %v, want sql.ErrNoRows", err)
		}
		if _, err := s.UserByName(ctx, "frank"); !errors.Is(err, ErrUserNotFound) {
			t.Errorf("UserByName after delete = %v, want ErrUserNotFound", err)
		}
	})
}

// TestTouchUserAndLastUsedRoundTrip verifies that a successful token lookup
// (both the v2 and legacy paths) records last_used_at, that TouchUser works
// directly, and that ListUsers surfaces the value.
func TestTouchUserAndLastUsedRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// A fresh user reports 0 (never used).
	tokV2, _, _ := createV2User(t, s, "alice")
	rows, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(rows) != 1 || rows[0].LastUsedAt != 0 {
		t.Fatalf("fresh user LastUsedAt = %+v, want 0", rows)
	}

	// A v2 token lookup touches the user.
	u, err := s.UserByToken(ctx, tokV2)
	if err != nil || u == nil {
		t.Fatalf("UserByToken: %v, %+v", err, u)
	}
	rows, err = s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers after v2 lookup: %v", err)
	}
	if rows[0].LastUsedAt == 0 {
		t.Errorf("LastUsedAt still 0 after successful v2 token lookup")
	}

	// Direct TouchUser round-trips (and returns nil for a missing id).
	if err := s.TouchUser(ctx, u.ID); err != nil {
		t.Fatalf("TouchUser: %v", err)
	}
	if err := s.TouchUser(ctx, 9999); err != nil {
		t.Errorf("TouchUser(unknown id) = %v, want nil (best-effort)", err)
	}

	// A legacy token lookup also touches its user.
	legacyToken := "old-plain-legacy-token"
	lh, err := auth.HashToken(legacyToken)
	if err != nil {
		t.Fatalf("HashToken: %v", err)
	}
	if _, err := s.CreateUser(ctx, "legacy-user", "", lh); err != nil {
		t.Fatalf("CreateUser (legacy): %v", err)
	}
	lu, err := s.UserByToken(ctx, legacyToken)
	if err != nil || lu == nil {
		t.Fatalf("UserByToken (legacy): %v, %+v", err, lu)
	}
	rows, err = s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers after legacy lookup: %v", err)
	}
	for _, r := range rows {
		if r.Name == "legacy-user" && r.LastUsedAt == 0 {
			t.Errorf("legacy user LastUsedAt still 0 after successful legacy lookup")
		}
	}
}
