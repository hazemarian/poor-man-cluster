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
	row, err := l.Store.Backup(ctx, id)
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
//
// The restore is local-first: an archive that still exists on this node's
// archive dir is restored from disk. When it is gone (pruned, or this node
// never held it) the archive is fetched from the configured offsite S3 store
// instead; a missing local archive with no S3 configured is a loud error that
// says where the archive lives.
func (l *Local) Restore(ctx context.Context, id int64, destRoot string, opts RestoreOptions) (int, error) {
	row, err := l.Store.Backup(ctx, id)
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
		src, cleanup, err := l.restoreSource(ctx, p, opts)
		if err != nil {
			return restored, err
		}
		if err := refuseControlPlaneArchive(src); err != nil {
			cleanup()
			return restored, err
		}
		var include func(string) bool
		if opts.Volume != "" {
			volume := opts.Volume
			if row.StackName.Valid && row.StackName.String != "" {
				// Stack-scoped run: anchor the volume to THIS app, so a
				// `--volume db_data` can never pull another app's same-named
				// volume out of a multi-stack archive.
				anchored := row.StackName.String + "/" + volume
				include = func(rel string) bool {
					return rel == anchored || strings.HasPrefix(rel, anchored+"/")
				}
			} else {
				// Whole-disk run: match by segment (a bare volume name matches
				// every app's volume; "<app>/<volume>" scopes to one app).
				include = func(rel string) bool { return volumeMatch(rel, volume) }
			}
		}
		n, err := restoreArchiveRelFiltered(src, target, archiveRelPath, include)
		cleanup()
		if err != nil {
			return restored, fmt.Errorf("restore %s: %w", p, err)
		}
		restored += n
	}
	return restored, nil
}

// restoreSource resolves where an archive's bytes come from: the local file
// (default), or a fetch from the configured offsite S3 store when the local
// copy is gone or --from-s3 is explicit. The returned cleanup removes any
// temporary file and is always safe to call.
func (l *Local) restoreSource(ctx context.Context, p string, opts RestoreOptions) (string, func(), error) {
	_, localErr := os.Stat(p)
	localExists := localErr == nil

	if opts.FromS3 && !l.S3.Configured() {
		return "", func() {}, fmt.Errorf("offsite S3 restore requested but no S3 is configured — set the backup_s3_* settings (endpoint, bucket, access key, secret key)")
	}
	if !opts.FromS3 && localExists {
		return p, func() {}, nil
	}
	if !opts.FromS3 && !l.S3.Configured() {
		return "", func() {}, fmt.Errorf("archive %s not found locally and no offsite S3 is configured — restore on the node that holds %s, or configure backup_s3_* settings to fetch it", p, l.ArchiveDir)
	}

	tmp, err := os.CreateTemp("", "pmcluster-restore-*.tar.gz")
	if err != nil {
		return "", func() {}, fmt.Errorf("temp file for s3 fetch: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	if err := fetchS3Object(ctx, l.S3, s3ObjectKey(p), tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", func() {}, err
	}
	return tmpPath, func() { _ = os.Remove(tmpPath) }, nil
}

// volumeMatch reports whether a restore-relative path belongs to the named
// volume. A volume name is matched as a full path segment (including the
// volume's own directory entry) so entries from different stacks or volumes
// never leak into a per-volume restore. An empty volume matches everything.
func volumeMatch(rel, volume string) bool {
	if volume == "" {
		return true
	}
	return rel == volume ||
		strings.HasPrefix(rel, volume+"/") ||
		strings.Contains(rel, "/"+volume+"/") ||
		strings.HasSuffix(rel, "/"+volume)
}

// refuseControlPlaneArchive inspects the first entry of an archive and errors
// when it belongs to the control-plane subtree (`backup/pmcluster`), whose
// data does not belong under the volume root.
func refuseControlPlaneArchive(p string) error {
	// Raw directory backups are never control-plane archives — short-circuit
	// before attempting to parse them as tar (which would fail with "is a
	// directory" and make directory restores impossible).
	if info, err := os.Stat(p); err == nil && info.IsDir() {
		return nil
	}
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

// ctlplaneRelPath maps a control-plane archive entry to its path relative to
// the pmcluster data dir. The control-plane agent archives ${DATA_DIR} under
// a baked-in `backup/pmcluster` prefix (source mount
// <data-dir>:/backup/pmcluster:ro), so that prefix is stripped.
func ctlplaneRelPath(name string) string {
	clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(name)), "/")
	const prefix = "backup/pmcluster"
	if clean == prefix {
		return "."
	}
	if strings.HasPrefix(clean, prefix+"/") {
		return strings.TrimPrefix(clean, prefix+"/")
	}
	return clean
}

// RestoreControlPlane extracts a control-plane backup archive back into the
// pmcluster data directory (the entry prefix `backup/pmcluster` is stripped,
// so data.db / .encryption_key / config/ land at the right places). Used by
// leader-aware failover: a standby manager that becomes swarm leader restores
// the newest control-plane archive before serving when its local DB is
// missing or older than the archive.
func RestoreControlPlane(p, dataDir string) (int, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return 0, err
	}
	return restoreArchiveRel(p, dataDir, ctlplaneRelPath)
}

// NewestControlPlaneArchive returns the control-plane archive in dir with the
// most recent modification time (i.e. the newest snapshot of ~/.pmcluster).
// Returns "" when none exist.
func NewestControlPlaneArchive(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var newest string
	var newestMod int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "pmcluster-ctlplane-") || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Unix() > newestMod {
			newestMod = info.ModTime().Unix()
			newest = filepath.Join(dir, e.Name())
		}
	}
	return newest, nil
}

// restoreArchiveRel extracts an archive into target, mapping every entry
// through rel (the prefix-stripping mapper). Raw directories are copied tree.
func restoreArchiveRel(p, target string, rel func(string) string) (int, error) {
	return restoreArchiveRelFiltered(p, target, rel, nil)
}

// restoreArchiveRelFiltered is restoreArchiveRel with an optional per-entry
// filter: entries whose mapped relative path is rejected by include are
// skipped. A nil include restores everything. Filtering applies to tar
// entries and to the recursive directory copy alike.
func restoreArchiveRelFiltered(p, target string, rel func(string) string, include func(string) bool) (int, error) {
	info, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	if info.IsDir() {
		// Raw directory backup: copy the tree.
		n, err := copyTreeFiltered(p, target, include)
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
		r := rel(hdr.Name)
		if include != nil && !include(r) {
			continue
		}
		name := filepath.Join(target, r)
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

// copyTreeFiltered copies a raw-directory backup tree into dst, skipping any
// subtree whose relative path is rejected by include (nil copies everything).
func copyTreeFiltered(src, dst string, include func(string) bool) (int, error) {
	var count int
	err := filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if rel == "." {
			return nil
		}
		if include != nil && !include(rel) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
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
