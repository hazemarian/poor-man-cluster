package backups

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseArtifactLegacyWholeNode(t *testing.T) {
	// Exactly the shape the offen agents write today: the node id is a swarm
	// id (no dashes), so the legacy parser in internal/stacks keeps working.
	const name = "backup-bh3e42i41uae6fd3mlkv7qfjg-2026-11-02T19-00-00.tar.gz"
	a, ok := ParseArtifact(name)
	if !ok {
		t.Fatalf("ParseArtifact(%q) = false, want true", name)
	}
	if a.Stack != "" {
		t.Errorf("Stack = %q, want empty for a whole-node archive", a.Stack)
	}
	if a.Node != "bh3e42i41uae6fd3mlkv7qfjg" {
		t.Errorf("Node = %q", a.Node)
	}
	if got, want := a.At.UTC(), time.Date(2026, 11, 2, 19, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("At = %v, want %v", got, want)
	}
}

func TestParseArtifactPerStack(t *testing.T) {
	for _, tc := range []struct {
		name  string
		node  string
		stack string
	}{
		{
			name:  "backup-bh3e42i41uae6fd3mlkv7qfjg-2026-11-02T19-00-00-stack-horizon-erp.tar.gz",
			node:  "bh3e42i41uae6fd3mlkv7qfjg",
			stack: "horizon-erp",
		},
		{
			// A stack name with several dashes must not be confused with the
			// node: the marker is the only split point.
			name:  "backup-bh3e42i41uae6fd3mlkv7qfjg-2026-11-02T19-00-00-stack-khaal-pvc-cms.tar.gz",
			node:  "bh3e42i41uae6fd3mlkv7qfjg",
			stack: "khaal-pvc-cms",
		},
		{
			// A hostname-shaped node (older archives) still parses, because the
			// timestamp is the anchor, not the first dash.
			name:  "backup-nxt-sw-1-m-2026-11-02T19-00-00-stack-dg-steps-erp.tar.gz",
			node:  "nxt-sw-1-m",
			stack: "dg-steps-erp",
		},
	} {
		a, ok := ParseArtifact(tc.name)
		if !ok {
			t.Fatalf("ParseArtifact(%q) = false", tc.name)
		}
		if a.Node != tc.node {
			t.Errorf("%s: Node = %q, want %q", tc.name, a.Node, tc.node)
		}
		if a.Stack != tc.stack {
			t.Errorf("%s: Stack = %q, want %q", tc.name, a.Stack, tc.stack)
		}
	}
}

func TestParseArtifactRejectsNonVolumeArchives(t *testing.T) {
	for _, name := range []string{
		"pmcluster-ctlplane-bh3e42i41uae6fd3mlkv7qfjg-2026-11-02T03-00-00.tar.gz",
		"purge-backup-2026-11-02.tar.gz",
		"backup-2026-11-02.tgz",
		"notes.txt",
		"backup--2026-11-02T19-00-00.tar.gz", // empty node
	} {
		if a, ok := ParseArtifact(name); ok {
			t.Errorf("ParseArtifact(%q) = %+v, want false", name, a)
		}
	}
}

func TestArtifactNameRoundTrips(t *testing.T) {
	at := time.Date(2026, 11, 2, 19, 0, 0, 0, time.UTC)
	name := ArtifactName("khaal-pvc-cms", "bh3e42i41uae6fd3mlkv7qfjg", at)
	if want := "backup-bh3e42i41uae6fd3mlkv7qfjg-2026-11-02T19-00-00-stack-khaal-pvc-cms.tar.gz"; name != want {
		t.Fatalf("ArtifactName = %q, want %q", name, want)
	}
	a, ok := ParseArtifact(name)
	if !ok || a.Stack != "khaal-pvc-cms" || a.Node != "bh3e42i41uae6fd3mlkv7qfjg" || !a.At.Equal(at) {
		t.Fatalf("round trip = %+v, ok=%v", a, ok)
	}
}

func TestArtifactNameSanitisesSegments(t *testing.T) {
	at := time.Date(2026, 11, 2, 19, 0, 0, 0, time.UTC)
	// A stack name containing the marker, a slash and whitespace must not be
	// able to forge a second marker or escape the archive directory.
	name := ArtifactName("my-stack-x/../../etc passwd", "node1", at)
	if filepath.Base(name) != name {
		t.Fatalf("name %q is not a leaf", name)
	}
	a, ok := ParseArtifact(name)
	if !ok {
		t.Fatalf("ParseArtifact(%q) = false", name)
	}
	if a.Stack == "" {
		t.Fatalf("stack lost: %+v", a)
	}
	// Exactly one marker survives sanitisation.
	if n := len(splitOnMarker(name)); n != 2 {
		t.Fatalf("marker count in %q = %d, want 1 occurrence", name, n-1)
	}
}

func splitOnMarker(s string) []string {
	out := []string{}
	rest := s
	for {
		i := indexOf(rest, stackMarker)
		if i < 0 {
			return append(out, rest)
		}
		out = append(out, rest[:i])
		rest = rest[i+len(stackMarker):]
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "backup-node1-2026-11-02T19-00-00-stack-app.tar.gz")
	if err := os.WriteFile(archive, []byte("tarbytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := Manifest{
		Stack:      "app",
		Node:       "node1",
		CreatedAt:  1700000000,
		Archive:    filepath.Base(archive),
		SizeBytes:  8,
		SHA256:     "deadbeef",
		VolumeRoot: "/var/stack/data",
		Prefix:     "backup/data/",
		Files:      3,
		Bytes:      12,
		Snapshot:   SnapshotFacts{Kind: "lvm", LV: "snap-1", PoolPercent: 4.5},
		Verify:     VerifyFacts{Verified: true, Engine: "sqlite", Detail: "integrity_check ok"},
	}
	if err := WriteManifest(archive, want); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	got, err := ReadManifest(archive)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.Stack != want.Stack || got.SHA256 != want.SHA256 || !got.Verify.Verified || got.Snapshot.LV != "snap-1" {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	// The sidecar must not look like an archive to a *.tar.gz scan.
	if IsArtifact(ManifestName(filepath.Base(archive))) {
		t.Fatal("manifest name must not parse as an archive")
	}
}

func TestReadManifestMissingIsNotExist(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadManifest(filepath.Join(dir, "backup-node1-2026-11-02T19-00-00.tar.gz"))
	if !os.IsNotExist(err) {
		t.Fatalf("err = %v, want os.ErrNotExist so callers can treat it as unverified", err)
	}
}
