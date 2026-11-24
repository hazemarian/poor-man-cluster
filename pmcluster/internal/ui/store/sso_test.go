package store

import (
	"context"
	"testing"
)

func TestEnsureSSOUserPersistsOnFirstLogin(t *testing.T) {
	st, err0 := Open(t.TempDir())
	if err0 != nil {
		t.Fatalf("open: %v", err0)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	u, err := st.EnsureSSOUser(ctx, "Someone@Example.COM", "someone")
	if err != nil {
		t.Fatalf("EnsureSSOUser: %v", err)
	}
	if u.Status != StatusPending || u.Role == RoleAdmin || u.AuthSource != AuthSSO {
		t.Fatalf("first sight = %+v, want pending sso non-admin", u)
	}
	again, err := st.ByEmail(ctx, "someone@example.com")
	if err != nil {
		t.Fatalf("row did not persist: %v", err)
	}
	if again.ID != u.ID {
		t.Fatalf("second read id = %d, want %d", again.ID, u.ID)
	}
	// A second login returns the same row, not a duplicate.
	third, err := st.EnsureSSOUser(ctx, "someone@example.com", "someone")
	if err != nil || third.ID != u.ID {
		t.Fatalf("second EnsureSSOUser = %+v, %v; want the same row", third, err)
	}
	pending, err := st.ListPending(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListPending = %d, %v; want 1", len(pending), err)
	}
}
