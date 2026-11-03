package cli

import (
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/storage"
)

func TestParseSizeAcceptsWhatOperatorsType(t *testing.T) {
	for raw, want := range map[string]int64{
		"32G":   32 << 30,
		"32GiB": 32 << 30,
		"512M":  512 << 20,
		"1T":    1 << 40,
		"2048":  2048,
		"1.5G":  int64(1.5 * float64(1<<30)),
	} {
		got, err := parseSize(raw)
		if err != nil {
			t.Fatalf("parseSize(%q): %v", raw, err)
		}
		if got != want {
			t.Fatalf("parseSize(%q) = %d, want %d", raw, got, want)
		}
	}
	for _, bad := range []string{"", "abc", "0", "-5G"} {
		if _, err := parseSize(bad); err == nil {
			t.Fatalf("parseSize(%q) accepted garbage", bad)
		}
	}
}

func TestStorageSpecFromSetupDefaultsToLoopWithAutoSize(t *testing.T) {
	spec, err := storageSpecFromSetup(setupAnswers{})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Mode != storage.ModeLoop {
		t.Fatalf("mode = %q, want loop", spec.Mode)
	}
	if spec.Size != 0 {
		t.Fatalf("size = %d, want 0 (auto)", spec.Size)
	}
}

func TestStorageSpecFromSetupHonoursNone(t *testing.T) {
	spec, err := storageSpecFromSetup(setupAnswers{StorageMode: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Mode != storage.ModeNone {
		t.Fatalf("mode = %q, want none", spec.Mode)
	}
}

func TestStorageSpecFromSetupRejectsABadSize(t *testing.T) {
	if _, err := storageSpecFromSetup(setupAnswers{StorageSize: "lots"}); err == nil {
		t.Fatal("accepted a nonsense size")
	} else if !strings.Contains(err.Error(), "invalid size") {
		t.Fatalf("error = %v", err)
	}
}
