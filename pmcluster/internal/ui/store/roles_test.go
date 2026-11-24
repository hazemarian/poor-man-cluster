package store

import (
	"context"
	"errors"
	"testing"
)

// TestRolesShipWithThePermissionSetsTheConsoleHad — an untouched cluster must
// behave exactly as it did before roles were data.
func TestRolesShipWithThePermissionSetsTheConsoleHad(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	viewer, err := st.RoleByName(ctx, RoleViewer)
	if err != nil {
		t.Fatalf("viewer: %v", err)
	}
	if !viewer.Has("console.view") || viewer.Has("stacks.operate") {
		t.Fatalf("viewer = %v, want read-only", viewer.Permissions)
	}
	op, _ := st.RoleByName(ctx, RoleOperator)
	if !op.Has("stacks.deploy") || op.Has("users.manage") {
		t.Fatalf("operator = %v, want deploy without account management", op.Permissions)
	}
	admin, _ := st.RoleByName(ctx, RoleAdmin)
	if len(admin.Permissions) != len(AllPermissions()) {
		t.Fatalf("admin has %d permissions, want all %d", len(admin.Permissions), len(AllPermissions()))
	}
}

// TestCreateRoleRejectsUnknownPermissions — a permission nobody checks would sit
// in the table granting nothing, and the editor would keep offering it.
func TestCreateRoleRejectsUnknownPermissions(t *testing.T) {
	st, _ := Open(t.TempDir())
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	if _, err := st.CreateRole(ctx, "deployer", "deploys", []string{"console.view", "stacks.deploy"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	r, err := st.RoleByName(ctx, "deployer")
	if err != nil || len(r.Permissions) != 2 {
		t.Fatalf("stored role = %+v, %v", r, err)
	}
	if _, err := st.CreateRole(ctx, "bogus", "", []string{"console.fly"}); err == nil {
		t.Fatal("an unknown permission was accepted")
	}
}

// TestDeleteRoleRefusesBuiltinsAndRolesInUse — deleting either would strand
// accounts that point at the name.
func TestDeleteRoleRefusesBuiltinsAndRolesInUse(t *testing.T) {
	st, _ := Open(t.TempDir())
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	if err := st.DeleteRole(ctx, RoleOperator); !errors.Is(err, ErrRoleBuiltin) {
		t.Fatalf("delete builtin = %v, want ErrRoleBuiltin", err)
	}
	if _, err := st.CreateRole(ctx, "temp", "", []string{"console.view"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.CreateUser(ctx, "someone", "hash", true, "temp"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.DeleteRole(ctx, "temp"); !errors.Is(err, ErrRoleInUse) {
		t.Fatalf("delete in-use role = %v, want ErrRoleInUse", err)
	}
}
