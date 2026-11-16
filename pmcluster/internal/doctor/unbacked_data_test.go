package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

func TestUnbackedData(t *testing.T) {
	mk := func(t *testing.T, root string, dirs map[string]int) {
		t.Helper()
		for name, files := range dirs {
			full := filepath.Join(root, name)
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", full, err)
			}
			for i := 0; i < files; i++ {
				// Distinct names: writing one name repeatedly is one entry, which is exactly
				// what an earlier version of this fixture got wrong.
				name := fmt.Sprintf("f-%d", i)
				if err := os.WriteFile(filepath.Join(full, name), []byte("x"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
		}
		// lost+found always exists on an ext4 volume; it must never be reported.
		_ = os.MkdirAll(filepath.Join(root, "lost+found"), 0o755)
	}
	docker := &nodeListOnly{nodes: nil, services: []runtime.Service{
		{Name: "shop_web", Stack: "shop"},
		{Name: "cms_web", Stack: "cms"},
	}}

	t.Run("a directory with data that belongs to no stack fails", func(t *testing.T) {
		root := t.TempDir()
		mk(t, root, map[string]int{"shop": 3, "dep-s2": 954, "dep-s3": 954})
		got := checkUnbackedData(context.Background(), backupView{VolumeRoot: root, Runtime: docker})
		if got.Status != StatusFail {
			t.Fatalf("status = %v (%s)", got.Status, got.Detail)
		}
		for _, want := range []string{"dep-s2", "954", "dep-s3"} {
			if !strings.Contains(got.Detail, want) {
				t.Errorf("detail does not mention %q: %s", want, got.Detail)
			}
		}
		if strings.Contains(got.Detail, "shop (") {
			t.Errorf("a live stack was reported as unbacked: %s", got.Detail)
		}
		if strings.Contains(got.Detail, "lost+found") {
			t.Errorf("lost+found was reported: %s", got.Detail)
		}
	})

	t.Run("live stacks only is ok", func(t *testing.T) {
		root := t.TempDir()
		mk(t, root, map[string]int{"shop": 2, "cms": 1})
		got := checkUnbackedData(context.Background(), backupView{VolumeRoot: root, Runtime: docker})
		if got.Status != StatusOK {
			t.Fatalf("status = %v (%s)", got.Status, got.Detail)
		}
	})

	t.Run("empty leftovers are named but not a failure", func(t *testing.T) {
		root := t.TempDir()
		mk(t, root, map[string]int{"shop": 1, "abbas": 0})
		got := checkUnbackedData(context.Background(), backupView{VolumeRoot: root, Runtime: docker})
		if got.Status != StatusOK {
			t.Fatalf("an empty leftover made the check fail: %v (%s)", got.Status, got.Detail)
		}
		if !strings.Contains(got.Detail, "abbas") {
			t.Errorf("the empty leftover is not named: %s", got.Detail)
		}
	})

	t.Run("no volume root skips", func(t *testing.T) {
		if got := checkUnbackedData(context.Background(), backupView{Runtime: docker}); got.Status != StatusSkip {
			t.Fatalf("status = %v", got.Status)
		}
	})
}

// TestCountFiles_IsBounded keeps a health check from becoming a walk of a 90 GiB volume.
func TestCountFiles_IsBounded(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "big")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := countFiles(dir); got != 1 {
		t.Errorf("countFiles counted %d for one file name written repeatedly, want 1 (it counts ENTRIES)", got)
	}
	if got := countFiles(filepath.Join(dir, "missing")); got != 0 {
		t.Errorf("countFiles on a missing dir = %d", got)
	}
}
