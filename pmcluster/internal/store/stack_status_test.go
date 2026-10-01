package store

import (
	"context"
	"testing"
)

func TestStackStatus_RoundTrip(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// Missing → ErrNotFound.
	if _, err := st.GetStackStatus(ctx, "demo"); err != ErrNotFound {
		t.Fatalf("GetStackStatus(missing) err = %v, want ErrNotFound", err)
	}

	// Set one, read back.
	svcs := map[string]string{"web": "healthy", "db": "degraded"}
	if err := st.SetStackStatus(ctx, StackStatus{
		StackName: "demo", Status: "degraded", Services: svcs, UpdatedAt: 100,
	}); err != nil {
		t.Fatalf("SetStackStatus: %v", err)
	}
	got, err := st.GetStackStatus(ctx, "demo")
	if err != nil {
		t.Fatalf("GetStackStatus: %v", err)
	}
	if got.Status != "degraded" || got.UpdatedAt != 100 || len(got.Services) != 2 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.Services["web"] != "healthy" || got.Services["db"] != "degraded" {
		t.Fatalf("service map mismatch: %v", got.Services)
	}

	// Upsert replaces, not duplicates.
	if err := st.SetStackStatus(ctx, StackStatus{
		StackName: "demo", Status: "healthy", Services: map[string]string{"web": "healthy"}, UpdatedAt: 200,
	}); err != nil {
		t.Fatalf("SetStackStatus upsert: %v", err)
	}
	got2, _ := st.GetStackStatus(ctx, "demo")
	if got2.Status != "healthy" || got2.UpdatedAt != 200 || len(got2.Services) != 1 {
		t.Fatalf("upsert mismatch: %+v", got2)
	}
}

func TestStackStatus_ListAndNilServices(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.SetStackStatus(ctx, StackStatus{StackName: "a", Status: "healthy", UpdatedAt: 1}); err != nil {
		t.Fatalf("set a: %v", err)
	}
	if err := st.SetStackStatus(ctx, StackStatus{StackName: "b", Status: "error", Services: map[string]string{"x": "error"}, UpdatedAt: 2}); err != nil {
		t.Fatalf("set b: %v", err)
	}
	all, err := st.ListStackStatuses(ctx)
	if err != nil {
		t.Fatalf("ListStackStatuses: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d rows, want 2", len(all))
	}
	// nil Services round-trips as an empty map, never null.
	if all[0].Services == nil {
		t.Fatalf("nil Services should round-trip as empty map: %+v", all[0])
	}
}
