package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSwapServesTheReplacedDatabase is the mechanism control-plane adoption
// depends on: the *Store pointer every consumer holds must keep working while
// the file behind it is replaced.
func TestSwapServesTheReplacedDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "data.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSetting(ctx, "probe", "old"); err != nil {
		t.Fatal(err)
	}

	// A replacement database, built the same way an adopted kit's data.db was.
	otherDir := t.TempDir()
	otherPath := filepath.Join(otherDir, "data.db")
	other, err := Open(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.SetSetting(ctx, "probe", "new"); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}

	if err := st.Swap(func() error { return os.Rename(otherPath, path) }); err != nil {
		t.Fatalf("swap: %v", err)
	}

	got, err := st.Setting(ctx, "probe")
	if err != nil {
		t.Fatalf("read through the reopened store: %v", err)
	}
	if got != "new" {
		t.Fatalf("probe = %q, want the replaced database's value", got)
	}
	// The handle is writable again, not merely readable.
	if err := st.SetSetting(ctx, "probe", "after"); err != nil {
		t.Fatalf("write through the reopened store: %v", err)
	}
}

// TestSwapReopensAfterFailedReplace: a refused adoption (fn returns an error)
// must leave the daemon serving the database that is still on disk.
func TestSwapReopensAfterFailedReplace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSetting(ctx, "probe", "kept"); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("replace refused")
	if err := st.Swap(func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("swap error = %v, want the fn's error", err)
	}
	got, err := st.Setting(ctx, "probe")
	if err != nil {
		t.Fatalf("the store must be usable after a failed replace: %v", err)
	}
	if got != "kept" {
		t.Fatalf("probe = %q, want the untouched value", got)
	}
}

// TestSwapOnClosedStore: a store with no open handle cannot swap, and says so
// instead of silently doing nothing.
func TestSwapOnClosedStore(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Swap(func() error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("swap on a closed store = %v, want ErrClosed", err)
	}
}
