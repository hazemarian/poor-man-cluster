package cluster

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarEntries returns the set of entry names in a tar.gz archive.
func tarEntries(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	out := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		out[hdr.Name] = true
	}
	return out
}

func TestWriteStoreBackup_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"data.db":         "sqlite-bytes",
		".encryption_key": "key-bytes",
		"config.yaml":     "data_dir: /tmp\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	path, err := WriteStoreBackup(dir)
	if err != nil {
		t.Fatalf("WriteStoreBackup: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("backup not created: %v", err)
	}

	if err := deleteStoreFiles(dir); err != nil {
		t.Fatalf("deleteStoreFiles: %v", err)
	}
	if err := RestoreStoreBackup(dir, path); err != nil {
		t.Fatalf("RestoreStoreBackup: %v", err)
	}

	for name, want := range map[string]string{
		"data.db":         "sqlite-bytes",
		".encryption_key": "key-bytes",
		"config.yaml":     "data_dir: /tmp\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read restored %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("restored %s = %q, want %q", name, got, want)
		}
	}
}

func TestWriteStoreBackup_SkipsMissingConfigYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.db"), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".encryption_key"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}

	path, err := WriteStoreBackup(dir)
	if err != nil {
		t.Fatalf("WriteStoreBackup: %v", err)
	}
	entries := tarEntries(t, path)
	if !entries["data.db"] || !entries[".encryption_key"] {
		t.Errorf("backup missing required files: %v", entries)
	}
	if entries["config.yaml"] {
		t.Errorf("backup should skip absent config.yaml: %v", entries)
	}
}

func TestRestoreStoreBackup_RejectsUnknownEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evil.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: "../evil", Mode: 0o600, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("evil")); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gw.Close()
	_ = f.Close()

	err = RestoreStoreBackup(dir, path)
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("expected refusing error, got %v", err)
	}
}
