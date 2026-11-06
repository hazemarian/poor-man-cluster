package backups

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A restored volume has to be the volume that was backed up, not a pile of
// files that happens to have the right names.
//
// Restore used to write every regular file as 0644 and to drop symlinks (and
// hard links) on the floor. A restored tree therefore looked complete and was
// not: private keys became world-readable, scripts lost +x, and `current ->
// releases/v3` vanished with no error at all. The drill links symlinks
// (verify.go) and always did — restore was the odd one out.
func TestRestoreKeepsModesSymlinksAndHardlinks(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "backup-node1-2026-01-01T00-00-00-stack-shop.tar.gz")

	f, err := os.Create(archive)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	write := func(hdr *tar.Header, body string) {
		t.Helper()
		hdr.Format = tar.FormatPAX
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", hdr.Name, err)
		}
		if body != "" {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatalf("write body %s: %v", hdr.Name, err)
			}
		}
	}

	write(&tar.Header{Name: "backup/data/", Typeflag: tar.TypeDir, Mode: 0o750}, "")
	write(&tar.Header{Name: "backup/data/erp.db", Typeflag: tar.TypeReg, Mode: 0o600, Size: 4}, "data")
	write(&tar.Header{Name: "backup/data/run.sh", Typeflag: tar.TypeReg, Mode: 0o755, Size: 6}, "#!/bin")
	write(&tar.Header{Name: "backup/data/current", Typeflag: tar.TypeSymlink, Linkname: "releases/v3", Mode: 0o777}, "")
	write(&tar.Header{Name: "backup/data/hard-a", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2}, "hi")
	write(&tar.Header{Name: "backup/data/hard-b", Typeflag: tar.TypeLink, Linkname: "backup/data/hard-a", Mode: 0o644}, "")
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	target := filepath.Join(dir, "restored")
	if _, err := restoreArchiveRel(archive, target, func(p string) string {
		return strings.TrimPrefix(p, "backup/data/")
	}); err != nil {
		t.Fatalf("restore: %v", err)
	}

	check := func(name string, want os.FileMode) {
		t.Helper()
		fi, err := os.Lstat(filepath.Join(target, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o, want %04o", name, got, want)
		}
	}
	check("erp.db", 0o600) // a private key or database must not become world-readable
	check("run.sh", 0o755) // an executable must stay executable
	check("hard-a", 0o644)

	link, err := os.Readlink(filepath.Join(target, "current"))
	if err != nil {
		t.Fatalf("symlink was not restored: %v", err)
	}
	if link != "releases/v3" {
		t.Errorf("symlink target = %q, want releases/v3", link)
	}
	a, err1 := os.Stat(filepath.Join(target, "hard-a"))
	b, err2 := os.Stat(filepath.Join(target, "hard-b"))
	if err1 != nil || err2 != nil {
		t.Fatalf("hard link was not restored: %v / %v", err1, err2)
	}
	if !os.SameFile(a, b) {
		t.Error("hard link was restored as a separate copy")
	}
}
