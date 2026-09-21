package backups

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// FileEntry is one item inside a backup archive: either a file on the host
// (when the backup run produced a raw directory) or an entry inside a
// tar/tar.gz archive produced by the offen backup agent.
type FileEntry struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
}

// Browse returns the run plus a listing of every archive it produced. For
// plain directories the listing walks the tree; for tar/tar.gz files it
// reads the archive headers. Entries are relative to each archive root.
func (l *Local) Browse(ctx context.Context, id int64) (*Run, []FileEntry, error) {
	row, err := l.Store.GetBackup(ctx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("get backup: %w", err)
	}
	run := runRows([]*store.Backup{row})[0]
	if run.Status == "pending" {
		return &run, nil, nil
	}
	var files []FileEntry
	for _, p := range run.ArchivePaths {
		entries, err := listArchive(p)
		if err != nil {
			// A missing archive is not fatal for browsing — surface the
			// path as an error entry so the UI can explain it.
			files = append(files, FileEntry{Path: p, IsDir: false, Size: -1})
			continue
		}
		files = append(files, entries...)
	}
	return &run, files, nil
}

func listArchive(p string) ([]FileEntry, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		var out []FileEntry
		err := filepath.Walk(p, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(p, path)
			if rel == "." {
				return nil
			}
			out = append(out, FileEntry{Path: rel, Size: fi.Size(), IsDir: fi.IsDir()})
			return nil
		})
		return out, err
	}
	// tar / tar.gz archive: read headers without extracting.
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var tr *tar.Reader
	switch {
	case strings.HasSuffix(p, ".gz"):
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("gzip open %s: %w", filepath.Base(p), err)
		}
		defer func() { _ = gz.Close() }()
		tr = tar.NewReader(gz)
	default:
		tr = tar.NewReader(f)
	}

	var out []FileEntry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar read %s: %w", filepath.Base(p), err)
		}
		out = append(out, FileEntry{Path: hdr.Name, Size: hdr.Size, IsDir: hdr.Typeflag == tar.TypeDir})
	}
	return out, nil
}

// Restore extracts every archive of a successful backup run back under
// destRoot (the volume root, e.g. /var/stack/data). Both stack-scoped and
// cluster-wide (whole-disk) runs are restorable: the offen agent archives
// the entire volume root with a baked-in `backup/data/` prefix, so entries
// are mapped back to destRoot after stripping that prefix. Control-plane
// archives (whose entries live under `backup/pmcluster`) are refused — their
// target is the control-plane data dir, not the volume root.
func (l *Local) Restore(ctx context.Context, id int64, destRoot string) (int, error) {
	row, err := l.Store.GetBackup(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("get backup: %w", err)
	}
	if row.Status != "succeeded" {
		return 0, fmt.Errorf("backup %d not restored: status is %q (only succeeded runs are restorable)", id, row.Status)
	}
	target := destRoot
	if err := os.MkdirAll(target, 0o755); err != nil {
		return 0, fmt.Errorf("create restore dir: %w", err)
	}
	var restored int
	for _, p := range splitArchivePaths(row.ArchivePaths) {
		if err := refuseControlPlaneArchive(p); err != nil {
			return restored, err
		}
		n, err := restoreArchive(p, target)
		if err != nil {
			return restored, fmt.Errorf("restore %s: %w", p, err)
		}
		restored += n
	}
	return restored, nil
}

// refuseControlPlaneArchive inspects the first entry of an archive and errors
// when it belongs to the control-plane subtree (`backup/pmcluster`), whose
// data does not belong under the volume root.
func refuseControlPlaneArchive(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	var tr *tar.Reader
	switch {
	case strings.HasSuffix(p, ".gz"):
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("gzip open: %w", err)
		}
		defer func() { _ = gz.Close() }()
		tr = tar.NewReader(gz)
	default:
		tr = tar.NewReader(f)
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(filepath.ToSlash(hdr.Name), "/")
		if strings.HasPrefix(name, "backup/pmcluster") {
			return fmt.Errorf("control-plane archive: cannot restore %s into the volume root", p)
		}
		if strings.HasPrefix(name, "backup/data") {
			return nil // first real entry confirms a data archive
		}
	}
}

// archiveRelPath maps an archive entry to its path relative to the restore
// target. The offen agent archives the volume root under a baked-in
// `backup/data/` prefix (source mount /var/stack/data:/backup/data:ro), so
// that prefix is stripped; entries without it are used as-is.
func archiveRelPath(name string) string {
	clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(name)), "/")
	const prefix = "backup/data"
	if clean == prefix {
		return "."
	}
	if strings.HasPrefix(clean, prefix+"/") {
		return strings.TrimPrefix(clean, prefix+"/")
	}
	return clean
}

func restoreArchive(p, target string) (int, error) {
	info, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	if info.IsDir() {
		// Raw directory backup: copy the tree.
		n, err := copyTree(p, target)
		return n, err
	}
	// tar / tar.gz archive.
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var tr *tar.Reader
	switch {
	case strings.HasSuffix(p, ".gz"):
		gz, err := gzip.NewReader(f)
		if err != nil {
			return 0, fmt.Errorf("gzip open: %w", err)
		}
		defer func() { _ = gz.Close() }()
		tr = tar.NewReader(gz)
	default:
		tr = tar.NewReader(f)
	}

	var count int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, err
		}
		rel := archiveRelPath(hdr.Name)
		name := filepath.Join(target, rel)
		if name != target && !strings.HasPrefix(name, target+string(filepath.Separator)) {
			return count, fmt.Errorf("archive entry escapes restore dir: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(name, 0o755); err != nil {
				return count, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return count, err
			}
			out, err := os.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return count, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return count, err
			}
			out.Close()
			count++
		}
	}
	return count, nil
}

func copyTree(src, dst string) (int, error) {
	var count int
	err := filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if rel == "." {
			return nil
		}
		out := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		o, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode())
		if err != nil {
			return err
		}
		if _, err := io.Copy(o, in); err != nil {
			o.Close()
			return err
		}
		o.Close()
		count++
		return nil
	})
	return count, err
}
