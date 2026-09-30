package store

import (
	"context"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
)

// TestMigration0020_AddsUserStackColumn verifies migration 0020 is applied
// and recorded: the users table grows a stack column whose default (”) is
// the unscoped value every pre-existing token keeps.
func TestMigration0020_AddsUserStackColumn(t *testing.T) {
	db := openRawDB(t)

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	var recorded int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM schema_version WHERE version = '0020_api_key_stack'`,
	).Scan(&recorded); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("0020_api_key_stack recorded %d times, want 1", recorded)
	}

	// The column must exist — a pre-migration-shaped INSERT (no stack) has
	// to be accepted and default to the unscoped ''.
	if _, err := db.Exec(
		`INSERT INTO users (name, token_id, token_hash, created_at) VALUES ('legacy', NULL, 'hash', 0)`,
	); err != nil {
		t.Fatalf("insert without stack column: %v", err)
	}
	var stack string
	if err := db.QueryRow(`SELECT stack FROM users WHERE name = 'legacy'`).Scan(&stack); err != nil {
		t.Fatalf("select stack: %v", err)
	}
	if stack != "" {
		t.Errorf("default stack = %q, want %q (unscoped)", stack, "")
	}
}

// TestMigration0020_AppliedOnOpen checks the same column through the public
// store path (Open runs the embedded migrations): a token created the old
// way — no stack argument — lands unscoped on a fresh data.db.
func TestMigration0020_AppliedOnOpen(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, "legacy", "", "hash"); err != nil {
		t.Fatalf("CreateUser (no stack arg): %v", err)
	}
	rows, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "legacy" {
		t.Fatalf("ListUsers = %+v, want single legacy row", rows)
	}
	if rows[0].Stack != "" {
		t.Errorf("legacy stack = %q, want %q (unscoped)", rows[0].Stack, "")
	}
}

// TestCreateUserWithStackScope covers the store-side scoping contract: a
// token created with a stack scope is retrieved with that scope (list, by
// id, by name, and — crucially — after a bearer-token lookup), while a
// token created without one stays unscoped.
func TestCreateUserWithStackScope(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	scopedTok, _, _ := createV2User(t, s, "ci", "demo")
	plainTok, _, _ := createV2User(t, s, "admin")

	t.Run("ListUsers surfaces the scope", func(t *testing.T) {
		rows, err := s.ListUsers(ctx)
		if err != nil {
			t.Fatalf("ListUsers: %v", err)
		}
		byName := map[string]string{}
		for _, r := range rows {
			byName[r.Name] = r.Stack
		}
		if byName["ci"] != "demo" {
			t.Errorf("ci stack = %q, want demo", byName["ci"])
		}
		if byName["admin"] != "" {
			t.Errorf("admin stack = %q, want %q (unscoped)", byName["admin"], "")
		}
	})

	t.Run("UserByToken carries the scope", func(t *testing.T) {
		u, err := s.UserByToken(ctx, scopedTok)
		if err != nil {
			t.Fatalf("UserByToken (scoped): %v", err)
		}
		if u == nil {
			t.Fatal("UserByToken (scoped) = nil user")
		}
		if u.Stack != "demo" {
			t.Errorf("UserByToken stack = %q, want demo", u.Stack)
		}

		up, err := s.UserByToken(ctx, plainTok)
		if err != nil {
			t.Fatalf("UserByToken (unscoped): %v", err)
		}
		if up == nil {
			t.Fatal("UserByToken (unscoped) = nil user")
		}
		if up.Stack != "" {
			t.Errorf("UserByToken stack = %q, want %q (unscoped)", up.Stack, "")
		}
	})

	t.Run("legacy token lookup carries the scope too", func(t *testing.T) {
		legacyToken := "old-plain-legacy-token"
		h, err := auth.HashToken(legacyToken)
		if err != nil {
			t.Fatalf("HashToken: %v", err)
		}
		if _, err := s.CreateUser(ctx, "old", "", h, "demo"); err != nil {
			t.Fatalf("CreateUser (legacy): %v", err)
		}
		u, err := s.UserByToken(ctx, legacyToken)
		if err != nil {
			t.Fatalf("UserByToken (legacy): %v", err)
		}
		if u == nil || u.Stack != "demo" {
			t.Fatalf("legacy lookup = %+v, want stack demo", u)
		}
	})

	t.Run("UserByID and UserByName surface the scope", func(t *testing.T) {
		var id int64
		if err := s.DB().QueryRow(`SELECT id FROM users WHERE name = 'ci'`).Scan(&id); err != nil {
			t.Fatalf("query id: %v", err)
		}
		byID, err := s.UserByID(ctx, id)
		if err != nil {
			t.Fatalf("UserByID: %v", err)
		}
		if byID.Stack != "demo" {
			t.Errorf("UserByID stack = %q, want demo", byID.Stack)
		}
		byName, err := s.UserByName(ctx, "ci")
		if err != nil {
			t.Fatalf("UserByName: %v", err)
		}
		if byName.Stack != "demo" {
			t.Errorf("UserByName stack = %q, want demo", byName.Stack)
		}
	})
}
