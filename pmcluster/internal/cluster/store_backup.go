package cluster

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// storeBackupPrefix is the filename prefix for the restorable safety backup
// `cluster down --purge` writes before deleting the local store.
const storeBackupPrefix = "purge-backup-"

// storeBackupFiles are the local-store files a purge backup preserves. The
// .encryption_key is REQUIRED — without it the encrypted secret material
// (managed credentials, DB secrets) is unreadable, so a restore would be
// useless. config.yaml is optional (it may not exist).
var storeBackupFiles = []string{"data.db", ".encryption_key", "config.yaml"}

// WriteStoreBackup writes a restorable tarball of the local store to
// <dir>/purge-backup-<RFC3339>.tar.gz containing data.db + .encryption_key
// (+ config.yaml when present) and returns the archive path. data.db and
// .encryption_key are required; config.yaml is skipped when absent. On any
// error the partial archive is removed so callers never mistake it for a good
// backup.
func WriteStoreBackup(dir string) (string, error) {
	ts := time.Now().UTC().Format(time.RFC3339)
	path := filepath.Join(dir, storeBackupPrefix+ts+".tar.gz")

	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("create store backup %s: %w", path, err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	fail := func(err error) (string, error) {
		_ = tw.Close()
		_ = gw.Close()
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}

	for _, name := range storeBackupFiles {
		src := filepath.Join(dir, name)
		data, err := os.ReadFile(src)
		if err != nil {
			if os.IsNotExist(err) && name == "config.yaml" {
				continue
			}
			return fail(fmt.Errorf("read %s for store backup: %w", src, err))
		}
		hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data))}
		if err := tw.WriteHeader(hdr); err != nil {
			return fail(fmt.Errorf("write tar header %s: %w", name, err))
		}
		if _, err := tw.Write(data); err != nil {
			return fail(fmt.Errorf("write tar entry %s: %w", name, err))
		}
	}

	if err := tw.Close(); err != nil {
		return fail(fmt.Errorf("close tar: %w", err))
	}
	if err := gw.Close(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("close gzip: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close store backup %s: %w", path, err)
	}
	return path, nil
}

// RestoreStoreBackup extracts a purge-backup tarball into dir, restoring the
// local store files (data.db + .encryption_key + config.yaml). It only accepts
// the three known store filenames and refuses traversal/absolute entries, so a
// crafted archive cannot write outside dir.
func RestoreStoreBackup(dir, archivePath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open backup %s: %w", archivePath, err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip backup %s: %w", archivePath, err)
	}
	defer func() { _ = gr.Close() }()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read backup entry: %w", err)
		}
		// Only bare filenames are allowed — this both pins the known set and
		// rejects ../ or absolute paths.
		known := false
		for _, name := range storeBackupFiles {
			if hdr.Name == name {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("refusing unknown backup entry %q", hdr.Name)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("read backup entry %s: %w", hdr.Name, err)
		}
		dst := filepath.Join(dir, hdr.Name)
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
	}
	return nil
}

// deleteStoreFiles removes the local store files (data.db, .encryption_key,
// config.yaml), leaving the backup archive intact. Missing files are ignored.
// Used by `cluster down --purge` to achieve a fully clean slate.
func deleteStoreFiles(dir string) error {
	for _, name := range storeBackupFiles {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove store file %s: %w", name, err)
		}
	}
	return nil
}
