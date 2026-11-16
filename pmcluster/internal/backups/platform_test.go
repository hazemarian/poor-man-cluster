package backups

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newPlatformFixture writes a data.db and a key into a temp data dir.
func newPlatformFixture(t *testing.T) (dataDir string, dbBytes, keyBytes []byte) {
	t.Helper()
	dataDir = t.TempDir()
	dbBytes = []byte("SQLite format 3\x00 this stands in for a control-plane database")
	keyBytes = []byte("0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(filepath.Join(dataDir, "data.db"), dbBytes, 0o644); err != nil {
		t.Fatalf("write data.db: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, ".encryption_key"), keyBytes, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return dataDir, dbBytes, keyBytes
}

// TestExportPlatform_IsSelfSufficient is the whole point of the artifact: the archive holds
// the database AND the key, so restoring it can decrypt the secrets it carries. A
// control-plane export without the key would look like a backup and restore into a cluster
// whose secrets nobody can read.
func TestExportPlatform_IsSelfSufficient(t *testing.T) {
	dataDir, dbBytes, keyBytes := newPlatformFixture(t)
	outDir := filepath.Join(t.TempDir(), "backup")
	now := time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC)

	res, err := ExportPlatform(PlatformExportOptions{
		DataDir: dataDir, OutDir: outDir, Hostname: "nxt-sw-5-m",
		Version: "v0.2.346", Commit: "abc1234", Now: now,
	})
	if err != nil {
		t.Fatalf("ExportPlatform: %v", err)
	}
	if want := "pmcluster-platform-20261116T093000Z.tar.gz"; res.Name != want {
		t.Errorf("name = %q, want %q (UTC, sortable)", res.Name, want)
	}
	if res.Bytes == 0 {
		t.Error("archive reported 0 bytes")
	}
	st, err := os.Stat(res.Path)
	if err != nil {
		t.Fatalf("stat archive: %v", err)
	}
	if perms := st.Mode().Perm(); perms != 0o600 {
		t.Errorf("archive mode = %04o, want 0600: it carries the cluster's key", perms)
	}

	contents := readArchive(t, res.Path)
	for _, want := range []string{"data.db", ".encryption_key", PlatformManifestName} {
		if _, ok := contents[want]; !ok {
			t.Fatalf("archive is missing %q; it has %v", want, keysOf(contents))
		}
	}
	if !bytes.Equal(contents["data.db"].body, dbBytes) {
		t.Error("data.db inside the archive differs from the file on disk")
	}
	if !bytes.Equal(contents[".encryption_key"].body, keyBytes) {
		t.Error("the key inside the archive differs from the file on disk")
	}
	if contents["data.db"].hdr.Mode != 0o644 {
		t.Errorf("data.db mode inside the archive = %04o, want 0644", contents["data.db"].hdr.Mode)
	}
	if contents[".encryption_key"].hdr.Mode != 0o600 {
		t.Errorf("key mode inside the archive = %04o, want 0600", contents[".encryption_key"].hdr.Mode)
	}

	// The manifest must describe the bytes that are actually in the archive, not what was
	// on disk before: that is what a human checks before restoring.
	m, err := ReadPlatformManifest(res.Path)
	if err != nil {
		t.Fatalf("ReadPlatformManifest: %v", err)
	}
	if m.Hostname != "nxt-sw-5-m" || m.Version != "v0.2.346" || m.Commit != "abc1234" {
		t.Errorf("manifest provenance = %+v, want the host and build it came from", m)
	}
	if m.CreatedAt != "2026-11-16T09:30:00Z" {
		t.Errorf("manifest created_at = %q", m.CreatedAt)
	}
	wantSums := map[string]string{
		"data.db":         sha(dbBytes),
		".encryption_key": sha(keyBytes),
	}
	got := map[string]string{}
	for _, mem := range m.Members {
		got[mem.Name] = mem.SHA256
		if mem.Bytes <= 0 {
			t.Errorf("%s recorded %d bytes", mem.Name, mem.Bytes)
		}
	}
	for name, want := range wantSums {
		if got[name] != want {
			t.Errorf("manifest sha256 for %s = %q, want %q", name, got[name], want)
		}
	}
	if len(m.Members) != 2 {
		t.Errorf("manifest lists %d members, want 2 (data.db and the key)", len(m.Members))
	}
}

// TestExportPlatform_RefusesWithoutTheKey: an export that cannot decrypt the secrets it
// claims to protect is worse than no export, because nobody finds out until the restore.
func TestExportPlatform_RefusesWithoutTheKey(t *testing.T) {
	dataDir, _, _ := newPlatformFixture(t)
	if err := os.Remove(filepath.Join(dataDir, ".encryption_key")); err != nil {
		t.Fatalf("remove key: %v", err)
	}
	outDir := filepath.Join(t.TempDir(), "backup")

	_, err := ExportPlatform(PlatformExportOptions{DataDir: dataDir, OutDir: outDir, Hostname: "h"})
	if err == nil {
		t.Fatal("ExportPlatform wrote an archive with no encryption key in it")
	}
	entries, _ := os.ReadDir(outDir)
	for _, e := range entries {
		t.Errorf("a refused export left %s behind; a partial archive reads as complete", e.Name())
	}
}

func TestExportPlatform_RefusesWithoutTheDatabase(t *testing.T) {
	dataDir, _, _ := newPlatformFixture(t)
	if err := os.Remove(filepath.Join(dataDir, "data.db")); err != nil {
		t.Fatalf("remove data.db: %v", err)
	}
	if _, err := ExportPlatform(PlatformExportOptions{DataDir: dataDir, OutDir: t.TempDir()}); err == nil {
		t.Fatal("ExportPlatform wrote an archive with no database in it")
	}
}

func TestExportPlatform_TwoExportsSortChronologically(t *testing.T) {
	dataDir, _, _ := newPlatformFixture(t)
	outDir := t.TempDir()
	first, err := ExportPlatform(PlatformExportOptions{DataDir: dataDir, OutDir: outDir,
		Now: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("first export: %v", err)
	}
	second, err := ExportPlatform(PlatformExportOptions{DataDir: dataDir, OutDir: outDir,
		Now: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("second export: %v", err)
	}
	if first.Name >= second.Name {
		t.Errorf("names do not sort chronologically: %q then %q", first.Name, second.Name)
	}
	if first.Path == second.Path {
		t.Error("the second export overwrote the first")
	}
}

func TestReadPlatformManifest_RejectsAFileThatIsNotOne(t *testing.T) {
	junk := filepath.Join(t.TempDir(), "junk.tar.gz")
	if err := os.WriteFile(junk, []byte("not a gzip stream"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPlatformManifest(junk); err == nil {
		t.Fatal("ReadPlatformManifest accepted a non-tar file")
	}
}

type member struct {
	hdr  *tar.Header
	body []byte
}

func readArchive(t *testing.T, path string) map[string]member {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer func() { _ = gz.Close() }()
	out := map[string]member{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = member{hdr: hdr, body: body}
	}
	return out
}

func keysOf(m map[string]member) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestPlatformManifest_IsValidJSON keeps the on-disk shape honest: it is read by humans and
// by the restore path, so a field rename must show up as a test failure rather than as a
// decode error during a recovery.
func TestPlatformManifest_IsValidJSON(t *testing.T) {
	body, err := json.Marshal(PlatformManifest{Kind: "pmcluster-platform-export"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back["kind"] != "pmcluster-platform-export" {
		t.Errorf("kind field = %v", back["kind"])
	}
}
