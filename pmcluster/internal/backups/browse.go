package backups

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// ErrNotAnArchive reports bytes that are not a tar or tar.gz stream at all: an
// empty body, an HTML error page, a JSON sidecar. It is deliberately distinct
// from a corrupt archive — nothing was ever an artifact here, which almost
// always means the wrong object was fetched (a key that resolves to an error
// page, a redirect that answered 200, a bucket with a prefix the key lacks).
var ErrNotAnArchive = errors.New("not an archive")

// classifyArchive reads the first block of a file and names what it is.
//
// Extraction used to trust the file name alone. A fetched object is written to
// a temp file with a hard-coded ".tar.gz" suffix, so every fetched object
// looked like an archive: an endpoint answering 200 with an HTML page failed
// deep inside the gzip reader, and an empty body parsed as a tar with no
// entries — which, before it was made an error, read as a successful restore
// of nothing. Naming the bytes up front turns both into one clear sentence.
func classifyArchive(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 512)
	n, rerr := io.ReadFull(f, head)
	if n == 0 {
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			return "", fmt.Errorf("%w: %s is empty (0 bytes)", ErrNotAnArchive, filepath.Base(p))
		}
		return "", rerr
	}
	head = head[:n]
	if len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b {
		return "gzip", nil
	}
	if len(head) >= 262 && string(head[257:262]) == "ustar" {
		return "tar", nil
	}
	if len(head) < 512 {
		// Short read and no magic: a fragment, not a stream we could parse.
		return "", fmt.Errorf("%w: %s is %d bytes and starts with %q, not a tar or gzip header",
			ErrNotAnArchive, filepath.Base(p), n, printable(head, 32))
	}
	return "", fmt.Errorf("%w: %s starts with %q, not a tar or gzip header",
		ErrNotAnArchive, filepath.Base(p), printable(head, 32))
}

// printable renders the first n bytes of a probe as a quoted, single-line
// string so an error can show what the fetch actually landed on.
func printable(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	var sb strings.Builder
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			sb.WriteByte(c)
		} else {
			sb.WriteByte('.')
		}
	}
	return strings.TrimSpace(sb.String())
}

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
		entries, err := listArchive(l.localArchive(p))
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

// listArchive lists an archive's entries. A file that is not an archive, or an
// archive with no entries at all, is an ERROR rather than an empty list: an
// empty list is indistinguishable from "the artifact holds nothing", and treating
// those as the same is how a restore that extracted nothing still reported
// success. Callers that can legitimately meet a missing file (browsing a pruned
// archive) get a plain os error and explain it themselves.
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
	if _, err := classifyArchive(p); err != nil {
		return nil, err
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
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s holds no entries (an empty or truncated archive)", ErrNotAnArchive, filepath.Base(p))
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
// The restore REPLACES rather than merges: once the archives are extracted,
// entries under the restored scope that the snapshot did not hold are removed,
// so the volume ends up as the snapshot captured it instead of as a union of
// two points in time. That distinction is the whole point for a database,
// where a lock file or an append-only log written after the snapshot describes
// a different moment than the data pages beside it. See pruneToSnapshot for
// what the scope is — it is never wider than what the snapshot covered, so an
// app deployed after the backup is not touched.
//
// Ordering is deliberate: the snapshot's entry set is read BEFORE anything is
// written, and every archive is extracted BEFORE anything is removed. A
// restore that dies on a truncated archive therefore leaves the volume as it
// was, instead of half-empty.
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
	// offsite-missing is restorable: the archive on disk is the same snapshot,
	// and restore is local-first. Only the offsite COPY of it is absent.
	if row.Status != "succeeded" && row.Status != StatusOffsiteMissing {
		return 0, fmt.Errorf("backup %d not restored: status is %q (only succeeded runs are restorable)", id, row.Status)
	}
	// A run can be recorded as succeeded with no archive path at all (the
	// agent's stdout yielded no path and no archive was found in the archive
	// dir). Restoring it used to restore nothing and return success, so the
	// caller — a script, a CI job, an operator mid-incident — saw the same
	// tick as a real restore. Refuse instead: there is no artifact to restore
	// FROM, which is a different fact from an archive that held no files.
	if len(splitArchivePaths(row.ArchivePaths)) == 0 {
		return 0, fmt.Errorf("backup %d has no archive path recorded, so there is nothing to restore from. The run is marked succeeded but no archive was discovered; re-run the backup, and check the archive dir on the node that took it (%s)", id, l.ArchiveDir)
	}
	target := destRoot
	if err := os.MkdirAll(target, 0o755); err != nil {
		return 0, fmt.Errorf("create restore dir: %w", err)
	}
	// keep is the snapshot's view of the tree: every entry it holds plus every
	// ancestor directory of one, unioned across the run's archives. Collected
	// in full before a single byte is written.
	keep := map[string]bool{}
	var restored, archived int
	var names []string
	for _, p := range splitArchivePaths(row.ArchivePaths) {
		src, cleanup, err := l.restoreSource(ctx, p, opts)
		if err != nil {
			return restored, err
		}
		if err := refuseControlPlaneArchive(src); err != nil {
			cleanup()
			return restored, err
		}
		// Verify before extracting, not after: a mismatched artifact is a wrong
		// artifact, and half of it is already on the user's disk by the time a
		// post-hoc check could complain.
		if _, verr := verifyArtifact(src, p); verr != nil {
			cleanup()
			return restored, fmt.Errorf("refusing to restore %s: %w", filepath.Base(p), verr)
		}
		include := restoreInclude(row, opts)
		files, err := collectArchivedPaths(src, include, keep)
		if err != nil {
			cleanup()
			return restored, fmt.Errorf("read archive %s: %w", p, err)
		}
		archived += files
		names = append(names, filepath.Base(p))
		n, err := restoreArchiveRelFiltered(src, target, archiveRelPath, include)
		cleanup()
		if err != nil {
			return restored, fmt.Errorf("restore %s: %w", p, err)
		}
		restored += n
	}

	// Only now that everything is on disk: drop what the snapshot did not
	// hold, inside the restored scope only.
	pruneToSnapshot(target, keep, opts.Volume != "")

	// Zero files has two very different causes, and the operator must be told
	// which one they are looking at: an artifact that holds no files at all is
	// an empty snapshot (the volume was empty when it was taken — the backup is
	// honest and the data was never there), whereas an artifact full of files
	// whose none matched means the scope was wrong. Saying "nothing matched the
	// restore scope" for the first case sends the operator hunting for a filter
	// bug when the real answer is that this node never held the data.
	if restored == 0 {
		if archived == 0 {
			return 0, fmt.Errorf("nothing was restored: %s contains only directories, so the snapshot captured an EMPTY volume — this is an empty backup, not a fetch or filter problem. Check that the stack's data was on the node that wrote it (%s) at the time of the run",
				strings.Join(names, ", "), l.ArchiveDir)
		}
		return 0, fmt.Errorf("nothing was restored: %s holds %d file(s), none of which matched the restore scope (volume %q) — the artifact was fetched intact, so the filter is what excluded everything",
			strings.Join(names, ", "), archived, opts.Volume)
	}
	return restored, nil
}

// restoreInclude returns the entry filter for opts.Volume, or nil to restore
// the whole run.
func restoreInclude(row *store.Backup, opts RestoreOptions) func(string) bool {
	if opts.Volume == "" {
		return nil
	}
	volume := opts.Volume
	if row != nil && row.StackName.Valid && row.StackName.String != "" {
		// Stack-scoped run: anchor the volume to THIS app, so a
		// `--volume db_data` can never pull another app's same-named volume
		// out of a multi-stack archive.
		anchored := row.StackName.String + "/" + volume
		return func(rel string) bool {
			return rel == anchored || strings.HasPrefix(rel, anchored+"/")
		}
	}
	// Whole-disk run: match by segment (a bare volume name matches every app's
	// volume; "<app>/<volume>" scopes to one app).
	return func(rel string) bool { return volumeMatch(rel, volume) }
}

// collectArchivedPaths adds an archive's entries — and every ancestor
// directory of one — to keep, mapped through archiveRelPath and narrowed by
// include. Reads the archive without extracting it. It also returns how many
// regular-file entries the archive holds in total, which is what lets a
// zero-file restore say whether the snapshot was empty or the filter excluded
// everything.
func collectArchivedPaths(src string, include func(string) bool, keep map[string]bool) (int, error) {
	entries, err := listArchive(src)
	if err != nil {
		return 0, err
	}
	files := 0
	for _, e := range entries {
		if !e.IsDir {
			files++
		}
		rel := filepath.ToSlash(archiveRelPath(e.Path))
		if rel == "" || rel == "." {
			continue
		}
		if include != nil && !include(rel) {
			continue
		}
		for {
			keep[rel] = true
			i := strings.LastIndexByte(rel, '/')
			if i < 0 {
				break
			}
			rel = rel[:i]
		}
	}
	return files, nil
}

// pruneToSnapshot removes destination entries the snapshot did not hold, under
// the restored scope only, and returns how many it removed.
//
// The scope is derived from the snapshot itself, never from the destination:
//
//   - with --volume the restore is confined to that volume, and a sibling
//     volume of the same app must survive, so the scope root is the volume dir
//     (<app>/<volume>);
//   - without it, every app the snapshot covered is rewound to the snapshot, so
//     the scope root is that app's dir. An app deployed after the backup is not
//     in the archive, is therefore never a scope root, and a whole-disk restore
//     cannot touch it.
//
// Nothing outside those roots is walked, so a restore can never reach another
// app's data. An entry that cannot be read is left where it is.
//
// NOTE: replacing is the behaviour as of today. Whether a restore should
// instead merge, or offer both, is still an open product decision — see
// docs/storage-and-databases.md. This is the one place that implements it.
func pruneToSnapshot(target string, keep map[string]bool, volumeScoped bool) int {
	depth := 1
	if volumeScoped {
		depth = 2
	}
	roots := map[string]bool{}
	for p := range keep {
		if strings.Count(p, "/")+1 < depth {
			continue
		}
		roots[strings.Join(strings.Split(p, "/")[:depth], "/")] = true
	}

	removed := 0
	for root := range roots {
		var doomed []string
		_ = filepath.Walk(filepath.Join(target, root), func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return nil // unreadable: leave it alone
			}
			rel, rerr := filepath.Rel(target, path)
			if rerr != nil {
				return nil
			}
			if keep[filepath.ToSlash(rel)] {
				return nil
			}
			doomed = append(doomed, path)
			if fi.IsDir() {
				return filepath.SkipDir // its whole subtree goes with it
			}
			return nil
		})
		for _, p := range doomed {
			if err := os.RemoveAll(p); err == nil {
				removed++
			}
		}
	}
	return removed
}

// localArchive resolves an archive path recorded on a backup row to a file on
// THIS host.
//
// An on-demand run records the path as the agent reported it — the CONTAINER
// path /archive/<name> — which does not exist on the host, while the file
// itself is in the archive dir. A row discovered from the archive dir or the
// object store already records a host path or a bare object key, so a path
// that exists is used unchanged. Without this mapping a local-first restore
// looks for /archive/<name> on a host that only has <ArchiveDir>/<name>,
// concludes the archive is missing, and says so — on the very node holding it.
func (l *Local) localArchive(p string) string {
	if p == "" {
		return p
	}
	if _, err := os.Stat(p); err == nil {
		return p
	}
	if l.ArchiveDir == "" {
		return p
	}
	if alt := filepath.Join(l.ArchiveDir, filepath.Base(p)); alt != p {
		if _, err := os.Stat(alt); err == nil {
			return alt
		}
	}
	return p
}

// restoreTempDir is where a fetched archive is staged while it is extracted.
//
// Not os.TempDir() by default: /tmp is a tmpfs on the reference hosts, which
// means RAM. Fetching a backup there caps restore at roughly "as big as the
// node's free memory" — a 900 MB artifact cannot be restored on a 1839 MB node
// even though its disk has 40 GB free — and the copy competes with the very
// pages the extractor is about to dirty. The archive dir is on disk, is where
// artifacts live anyway, and is the same filesystem the volumes usually sit on.
//
// The staging file lives in a dot SUBDIRECTORY of it, never beside the
// artifacts: archive discovery walks the archive dir for *.tar.gz and would
// otherwise index a half-written temp file as a backup. PMCLUSTER_RESTORE_TMPDIR
// overrides the base; "" falls back to os.CreateTemp's own default.
func (l *Local) restoreTempDir() string {
	if d := os.Getenv("PMCLUSTER_RESTORE_TMPDIR"); d != "" {
		return d
	}
	if l.ArchiveDir != "" {
		if fi, err := os.Stat(l.ArchiveDir); err == nil && fi.IsDir() {
			stage := filepath.Join(l.ArchiveDir, ".restore-tmp")
			if err := os.MkdirAll(stage, 0o700); err == nil {
				return stage
			}
		}
	}
	return ""
}

// restoreSource resolves where an archive's bytes come from: the local file
// (default), or a fetch from the configured offsite S3 store when the local
// copy is gone or --from-s3 is explicit. The returned cleanup removes any
// temporary file and is always safe to call.
func (l *Local) restoreSource(ctx context.Context, p string, opts RestoreOptions) (string, func(), error) {
	local := l.localArchive(p)
	_, localErr := os.Stat(local)
	localExists := localErr == nil

	if opts.FromS3 && !l.S3.Configured() {
		return "", func() {}, fmt.Errorf("offsite S3 restore requested but no S3 is configured — set the backup_s3_* settings (endpoint, bucket, access key, secret key)")
	}
	if !opts.FromS3 && localExists {
		return local, func() {}, nil
	}
	if !opts.FromS3 && !l.S3.Configured() {
		return "", func() {}, fmt.Errorf("archive %s not found locally and no offsite S3 is configured — restore on the node that holds %s, or configure backup_s3_* settings to fetch it", p, l.ArchiveDir)
	}

	tmp, err := os.CreateTemp(l.restoreTempDir(), "pmcluster-restore-*.tar.gz")
	if err != nil {
		return "", func() {}, fmt.Errorf("temp file for s3 fetch: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	key := s3ObjectKey(p)
	if err := fetchS3Object(ctx, l.S3, key, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", func() {}, err
	}
	// The sidecar travels with the artifact. Its absence is not fatal (older
	// artifacts have none), its presence is what lets the restore below prove
	// that the bytes it downloaded are the bytes that were taken.
	sidecar := ManifestName(tmpPath)
	if merr := fetchS3Object(ctx, l.S3, ManifestName(key), sidecar); merr != nil {
		_ = os.Remove(sidecar)
	} else if m, rerr := ReadManifest(tmpPath); rerr != nil || m.Archive != filepath.Base(key) {
		// A store that answers every GET with the object itself, or an old
		// sidecar for a different name, must not become a "manifest" this
		// restore then trusts. Keep it only when it describes THIS archive.
		_ = os.Remove(sidecar)
	}
	// The object downloaded, but that is not the same as "this is the artifact".
	// A key that resolves to an HTML error page, a JSON sidecar or an empty body
	// answers 200 and used to surface as a parse failure deep in gzip — or, for
	// an empty body, as a restore of zero files that looked like a restore.
	if _, err := classifyArchive(tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", func() {}, fmt.Errorf("offsite artifact %s from bucket %q on %s: %w (the object exists but is not a backup archive — check the bucket's key layout and that the endpoint is not serving an error page)",
			key, l.S3.Bucket, l.S3.Endpoint, err)
	}
	return tmpPath, func() { _ = os.Remove(tmpPath) }, nil
}

// verifyArtifact checks an artifact against its sidecar, trying the local copy
// when the first path is a temp fetch that arrived without one.
//
// A missing or unreadable sidecar is "unchecked", not a failure: artifacts
// written before sidecars existed must still restore.
func verifyArtifact(src, p string) (bool, error) {
	checked, err := VerifyArchive(src)
	if err != nil || checked || src == p {
		return checked, err
	}
	return VerifyArchive(p)
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
			// The archive records the real mode; the MkdirAll default is only a
			// placeholder for directories that did not exist yet.
			_ = os.Chmod(name, hdr.FileInfo().Mode().Perm())
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return count, err
			}
			// A dropped symlink is a silently missing file: the tree looks
			// complete and is not. The drill has always linked them (verify.go);
			// restore now matches it.
			_ = os.Remove(name)
			if err := os.Symlink(hdr.Linkname, name); err != nil {
				return count, fmt.Errorf("symlink %s -> %s: %w", r, hdr.Linkname, err)
			}
			count++
		case tar.TypeLink:
			if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return count, err
			}
			// Hard links carry their target as an archive-relative path, so the
			// same containment check applies before anything is linked.
			linkTarget := filepath.Join(target, rel(hdr.Linkname))
			if linkTarget != target && !strings.HasPrefix(linkTarget, target+string(filepath.Separator)) {
				return count, fmt.Errorf("archive hard link escapes restore dir: %s", hdr.Linkname)
			}
			_ = os.Remove(name)
			if err := os.Link(linkTarget, name); err != nil {
				return count, fmt.Errorf("hard link %s -> %s: %w", r, hdr.Linkname, err)
			}
			count++
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return count, err
			}
			mode := hdr.FileInfo().Mode().Perm()
			// Create private and widen afterwards: opening at the archive's mode
			// would publish a 0600 key as 0644 for as long as the copy takes.
			out, err := os.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return count, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return count, err
			}
			out.Close()
			if err := os.Chmod(name, mode); err != nil {
				return count, fmt.Errorf("chmod %s to %04o: %w", r, mode, err)
			}
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
		// A raw-directory backup is a copy of a live tree, so it can hold links.
		// Dropping them loses files exactly as it does for tar archives.
		if fi.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(out)
			if err := os.Symlink(linkTarget, out); err != nil {
				return fmt.Errorf("symlink %s -> %s: %w", rel, linkTarget, err)
			}
			count++
			return nil
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
