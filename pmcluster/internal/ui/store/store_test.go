package store

import (
	"context"
	"path/filepath"
	"testing"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestUserRoleCRUD(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	// CreateUser defaults to operator; explicit roles are stored.
	admin, err := s.CreateUser(ctx, "alice", "hash-a", true, RoleAdmin)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if admin.Role != RoleAdmin {
		t.Errorf("admin role = %q, want %q", admin.Role, RoleAdmin)
	}
	op, err := s.CreateUser(ctx, "bob", "hash-b", true, "")
	if err != nil {
		t.Fatalf("create operator: %v", err)
	}
	if op.Role != RoleOperator {
		t.Errorf("default role = %q, want %q", op.Role, RoleOperator)
	}
	viewer, err := s.CreateUser(ctx, "carol", "hash-c", true, RoleViewer)
	if err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	if viewer.Role != RoleViewer {
		t.Errorf("viewer role = %q, want %q", viewer.Role, RoleViewer)
	}

	// CountAdmins only counts admins.
	n, err := s.CountAdmins(ctx)
	if err != nil {
		t.Fatalf("CountAdmins: %v", err)
	}
	if n != 1 {
		t.Errorf("CountAdmins = %d, want 1", n)
	}

	// ListUsers ordered by id.
	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("ListUsers len = %d, want 3", len(users))
	}
	if users[0].Username != "alice" || users[1].Username != "bob" || users[2].Username != "carol" {
		t.Errorf("ListUsers order = [%s %s %s], want [alice bob carol]", users[0].Username, users[1].Username, users[2].Username)
	}

	// GetByID.
	got, err := s.GetByID(ctx, op.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Username != "bob" || got.Role != RoleOperator {
		t.Errorf("GetByID = %+v, want bob/operator", got)
	}

	// UpdateUser: role-only change leaves password untouched.
	if err := s.UpdateUser(ctx, op.ID, RoleViewer, ""); err != nil {
		t.Fatalf("update role: %v", err)
	}
	got, _ = s.GetByID(ctx, op.ID)
	if got.Role != RoleViewer || got.PasswordHash != "hash-b" {
		t.Errorf("after role-only update = %+v", got)
	}

	// UpdateUser: password change marks password_set.
	if err := s.UpdateUser(ctx, op.ID, RoleOperator, "hash-b2"); err != nil {
		t.Fatalf("update password: %v", err)
	}
	got, _ = s.GetByID(ctx, op.ID)
	if got.Role != RoleOperator || got.PasswordHash != "hash-b2" || !got.PasswordSet {
		t.Errorf("after password update = %+v", got)
	}

	// DeleteUser + ErrNotFound.
	if err := s.DeleteUser(ctx, viewer.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteUser(ctx, viewer.ID); err != ErrNotFound {
		t.Errorf("double delete err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetByID(ctx, viewer.ID); err != ErrNotFound {
		t.Errorf("GetByID after delete err = %v, want ErrNotFound", err)
	}
}

func TestUserRole_MigrationPromotesFirstUser(t *testing.T) {
	// A DB created before the role column existed (no role in CREATE TABLE)
	// must get the column added and the lowest-ID user promoted to admin.
	dataDir := t.TempDir()
	db, err := Open(dataDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()

	// Insert a legacy row the way the old schema would (no role column).
	if _, err := db.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, password_set, created_at)
		 VALUES ('legacy', 'h', 1, 1)`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	// Drop the role column to simulate a pre-RBAC DB.
	if _, err := db.db.ExecContext(ctx, `ALTER TABLE users DROP COLUMN role`); err != nil {
		t.Fatalf("drop role: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := Open(dataDir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	u, err := db2.GetByUsername(ctx, "legacy")
	if err != nil {
		t.Fatalf("GetByUsername: %v", err)
	}
	if u.Role != RoleAdmin {
		t.Errorf("migrated role = %q, want %q", u.Role, RoleAdmin)
	}
}

func TestCreateEnvUser_AdminRoleAndIdempotent(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	created, err := s.CreateEnvUser(ctx, "envuser", "hash")
	if err != nil {
		t.Fatalf("CreateEnvUser: %v", err)
	}
	if !created {
		t.Errorf("first CreateEnvUser created = false, want true")
	}
	u, err := s.GetByUsername(ctx, "envuser")
	if err != nil {
		t.Fatalf("GetByUsername: %v", err)
	}
	if u.Role != RoleAdmin {
		t.Errorf("env user role = %q, want admin", u.Role)
	}

	created, err = s.CreateEnvUser(ctx, "envuser", "hash2")
	if err != nil {
		t.Fatalf("second CreateEnvUser: %v", err)
	}
	if created {
		t.Errorf("second CreateEnvUser created = true, want false (idempotent)")
	}
}

func TestSeedSettingOnce_SeedsWhenAbsent(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	if err := s.SeedSettingOnce(ctx, KeyToken, "pmc_abc"); err != nil {
		t.Fatalf("SeedSettingOnce: %v", err)
	}
	got, err := s.GetSetting(ctx, KeyToken)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if got != "pmc_abc" {
		t.Errorf("value = %q, want %q", got, "pmc_abc")
	}
}

// TestSeedSettingOnce_DoesNotOverwriteExisting verifies the "store once"
// contract: once a key exists — including an operator's explicit clear from
// Settings (SetSetting(key, "")) — a subsequent seed must not overwrite it.
func TestSeedSettingOnce_DoesNotOverwriteExisting(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	if err := s.SeedSettingOnce(ctx, KeyToken, "first"); err != nil {
		t.Fatalf("seed first: %v", err)
	}
	if err := s.SeedSettingOnce(ctx, KeyToken, "second"); err != nil {
		t.Fatalf("seed second: %v", err)
	}

	got, err := s.GetSetting(ctx, KeyToken)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if got != "first" {
		t.Errorf("value = %q, want %q (must be stored once, not reseeded)", got, "first")
	}

	if err := s.SetSetting(ctx, KeyToken, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := s.SeedSettingOnce(ctx, KeyToken, "third"); err != nil {
		t.Fatalf("seed after clear: %v", err)
	}
	got, err = s.GetSetting(ctx, KeyToken)
	if err != nil {
		t.Fatalf("GetSetting after clear: %v", err)
	}
	if got != "" {
		t.Errorf("value after clear = %q, want empty (explicit clear must be respected)", got)
	}
}
