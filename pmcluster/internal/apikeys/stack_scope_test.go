package apikeys

import (
	"context"
	"testing"
)

// TestLocalCreateWithStackScope verifies the domain-level scoping surface:
// Create(name, stack) mints a token whose scope is persisted and visible on
// List, while Create(name) keeps the historical unscoped behaviour.
func TestLocalCreateWithStackScope(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	svc := NewLocal(st)

	scopedID, scopedTok, err := svc.Create(ctx, "ci", "demo")
	if err != nil {
		t.Fatalf("Create(scoped): %v", err)
	}
	if scopedID <= 0 || scopedTok == "" {
		t.Fatalf("Create(scoped) = %d, %q", scopedID, scopedTok)
	}

	plainID, _, err := svc.Create(ctx, "admin")
	if err != nil {
		t.Fatalf("Create(unscoped): %v", err)
	}
	if plainID <= 0 {
		t.Fatalf("Create(unscoped) id = %d", plainID)
	}

	keys, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byName := map[string]APIKey{}
	for _, k := range keys {
		byName[k.Name] = k
	}
	if k, ok := byName["ci"]; !ok || k.Stack != "demo" {
		t.Errorf("ci key = %+v, want stack demo", k)
	}
	if k, ok := byName["admin"]; !ok || k.Stack != "" {
		t.Errorf("admin key = %+v, want unscoped (empty stack)", k)
	}

	// The scope must survive the bearer-token lookup the middleware relies on.
	u, err := st.UserByToken(ctx, scopedTok)
	if err != nil {
		t.Fatalf("UserByToken: %v", err)
	}
	if u == nil {
		t.Fatal("UserByToken returned nil user")
	}
	if u.Stack != "demo" {
		t.Errorf("UserByToken stack = %q, want demo", u.Stack)
	}
}
