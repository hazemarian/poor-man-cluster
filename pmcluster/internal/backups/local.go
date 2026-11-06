package backups

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// Local runs the on-demand volume backup pipeline: it records the run in
// the audit table, executes the backup trigger and finalizes the row with
// the resulting archive paths.
type Local struct {
	Store *store.Store
	Run   func(ctx context.Context) ([]string, error)
	// ArchiveDir is the host directory the offen backup service writes
	// tarballs into (usually /var/stack/backup). When set, List and
	// ListForStack first reconcile scheduled offen runs that never passed
	// through Trigger, so the console and CLI see the full backup picture.
	// Empty disables discovery (used by tests and legacy callers).
	ArchiveDir string
	// RetentionDays is the sparse retention window used by Prune when the
	// caller passes no policy of its own. Zero means DefaultRetention's.
	// It does NOT cause anything to be deleted on a read: see Prune.
	RetentionDays int
	// S3 is the object store used for reads: restore fallback and cross-node
	// transit. It is the in-cluster SeaweedFS store when that is enabled, and
	// the offsite settings otherwise.
	S3 S3Config
	// Offsite is ONLY the external backup_s3_* destination. Discovery uses it
	// to decide whether a scheduled run's offsite copy actually landed: the
	// agent writes the local copy first, so an archive on disk is not evidence
	// that the offsite leg succeeded. Empty disables that verification (every
	// discovered run is recorded as succeeded, as before).
	Offsite S3Config
}

// DefaultArchiveDir is the archive root mounted into the bundled
// backup-stack services (/var/stack/backup on the host).
const DefaultArchiveDir = "/var/stack/backup"

// NewLocal returns a Service bound to the given store and trigger. The
// trigger may be nil; List/ListForStack still work, Trigger returns an
// error describing that the trigger is not configured.
func NewLocal(st *store.Store, trigger func(ctx context.Context) ([]string, error)) Service {
	return &Local{Store: st, Run: trigger}
}

// discover reconciles the audit table against BOTH the local archive dir and
// the in-cluster object store. It never fails the caller: a missing or
// unreadable archive dir, or an unreachable store, simply records nothing.
func (l *Local) discover(ctx context.Context) {
	if l.Store == nil {
		return
	}
	// Index the offsite bucket once per pass. Discovery is the only place that
	// can tell an operator the offsite copy of a scheduled run never landed,
	// because scheduled runs never pass through Trigger.
	var offsite map[string]bool
	var offsiteErr error
	if l.Offsite.Configured() {
		offsite, offsiteErr = l.offsiteKeys(ctx)
	}
	if l.ArchiveDir != "" {
		l.discoverLocal(ctx, offsite, offsiteErr)
	}
	if l.S3.Configured() {
		l.discoverStore(ctx, offsite, offsiteErr)
	}
}

// offsiteKeys lists the object names present in the configured offsite bucket,
// keyed by basename (offen names the offsite object after the local archive,
// so the basename is the join key between the two copies).
func (l *Local) offsiteKeys(ctx context.Context) (map[string]bool, error) {
	objects, err := listS3Objects(ctx, l.Offsite, "")
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(objects))
	for _, o := range objects {
		out[filepath.Base(o.Key)] = true
	}
	return out, nil
}

// offsiteVerdict returns the status and note for a newly discovered run whose
// archive is named base. With no offsite configured every run is a success, as
// before. With one configured, a run is only a success when its offsite object
// is present; an offsite copy that is provably absent — or a bucket that could
// not be read — is recorded as status offsite-missing with a note saying which
// of the two it was. Claiming success either way is what let a deleted bucket
// read as a clean backup history for hours.
func (l *Local) offsiteVerdict(base string, index map[string]bool, indexErr error) (string, string) {
	if !l.Offsite.Configured() {
		return StatusSucceeded, ""
	}
	if indexErr != nil {
		return StatusOffsiteMissing, fmt.Sprintf("offsite copy unverified: %v", indexErr)
	}
	if !index[base] {
		return StatusOffsiteMissing, fmt.Sprintf("offsite copy missing: %s is not in bucket %q on %s (the archive on disk is intact and restorable)", base, l.Offsite.Bucket, l.Offsite.Endpoint)
	}
	return StatusSucceeded, ""
}

// discoverLocal records scheduled offen archives found on disk that the audit
// table does not know about yet.
func (l *Local) discoverLocal(ctx context.Context, offsite map[string]bool, offsiteErr error) {
	entries, err := os.ReadDir(l.ArchiveDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		archivePath := filepath.Join(l.ArchiveDir, e.Name())
		// Skip tarballs an on-demand run already recorded (those rows have
		// an empty filename, so the filename unique index cannot dedupe).
		exists, err := l.Store.BackupExistsByPath(ctx, archivePath)
		if err != nil || exists {
			continue
		}
		// The unique index on filename makes this a no-op for tarballs
		// that already produced a row. Record the offsite verdict rather than
		// asserting success: the local copy is written first.
		status, note := l.offsiteVerdict(e.Name(), offsite, offsiteErr)
		// A per-stack artifact names its app, so the console can list it under
		// that stack; a whole-node archive leaves the stack empty, as before.
		stack := ""
		if a, ok := ParseArtifact(e.Name()); ok {
			stack = a.Stack
		}
		if note == "" {
			note = artifactVerdictNote(archivePath)
		}
		if err := l.Store.RecordDiscoveredBackupArtifact(ctx, e.Name(), archivePath, info.ModTime().Unix(), status, note, stack); err != nil {
			continue
		}
	}
}

// discoverStore indexes the in-cluster object store. The hourly cron archives
// produced on the storage nodes land in the store but never pass through
// Trigger, so a daemon/CLI running on a non-storage manager would otherwise
// never see them. Each discovered volume archive is recorded as a DB row
// whose archive path is the bare object key; restoreSource then resolves it
// through fetchS3Object (s3ObjectKey of a bare key is the key itself).
//
// Only volume archives (`backup-` prefix) are indexed: control-plane archives
// are restored through a separate path (RestoreControlPlane), never as volume
// data, so they are deliberately excluded. Nothing is ever deleted from the
// store.
func (l *Local) discoverStore(ctx context.Context, offsite map[string]bool, offsiteErr error) {
	objects, err := listS3Objects(ctx, l.S3, "backup-")
	if err != nil {
		return // an unreachable store must not fail List/ListForStack
	}
	for _, o := range objects {
		base := filepath.Base(o.Key)
		if !strings.HasSuffix(base, ".tar.gz") {
			continue
		}
		// Dedupe: an on-demand/trigger row stores the container path
		// (/archive/<base>) and a local discovery row stores the full host
		// path (<dir>/<base>); both contain <base> as a substring, so the
		// bare key catches them. The filename unique index is a second layer.
		exists, err := l.Store.BackupExistsByPath(ctx, base)
		if err != nil || exists {
			continue
		}
		at := time.Now().Unix()
		if !o.LastModified.IsZero() {
			at = o.LastModified.Unix()
		}
		status, note := l.offsiteVerdict(base, offsite, offsiteErr)
		if err := l.Store.RecordDiscoveredBackupStatus(ctx, base, base, at, status, note); err != nil {
			continue
		}
	}
}

// Trigger records the backup run, executes the trigger and finalizes the row.
func (l *Local) Trigger(ctx context.Context, stackName string, revision int64) (int64, []string, error) {
	id, err := l.Store.CreateBackup(ctx, stackName, revision)
	if err != nil {
		return 0, nil, fmt.Errorf("record backup: %w", err)
	}
	if l.Run == nil {
		return id, nil, ErrTriggerNotConfigured
	}
	started := time.Now().Unix()
	paths, err := l.Run(ctx)
	if err != nil {
		_ = l.Store.FinishBackup(ctx, id, "failed", joinArchivePaths(paths), err.Error())
		RecordOutcome(ctx, KindOnDemand, StatusFailed)
		return id, paths, err
	}
	// offen's stdout is not a stable contract across versions: when no
	// /archive/*.tar.gz token came back, fall back to the archive actually
	// created on disk during this run so the row stays browseable.
	if len(paths) == 0 && l.ArchiveDir != "" {
		if p := l.archiveCreatedAfter(ctx, started); p != "" {
			paths = []string{p}
		}
	}
	if err := l.Store.FinishBackup(ctx, id, "succeeded", joinArchivePaths(paths), ""); err != nil {
		return id, paths, fmt.Errorf("record backup finish: %w", err)
	}
	RecordOutcome(ctx, KindOnDemand, StatusSucceeded)
	return id, paths, nil
}

// archiveCreatedAfter returns the path of the newest .tar.gz in ArchiveDir
// whose mtime is at or after the given unix time, or "" when none matches.
func (l *Local) archiveCreatedAfter(ctx context.Context, at int64) string {
	entries, err := os.ReadDir(l.ArchiveDir)
	if err != nil {
		return ""
	}
	var newest string
	var newestMtime int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		m := info.ModTime().Unix()
		if m >= at && m >= newestMtime {
			newest, newestMtime = e.Name(), m
		}
	}
	if newest == "" {
		return ""
	}
	return filepath.Join(l.ArchiveDir, newest)
}

// Prune applies the retention plan to this node's archive directory and then
// removes the audit rows of the artifacts it actually deleted.
//
// It is deliberately NOT called from List/ListForStack. Those used to prune, so
// *opening the console's backups page deleted archives*: a flat age cut that
// ignored KeepLast (the newest copies of a stack) and IncludeLegacy (whole-node
// archives, which are the only backups of a node that has not been migrated —
// planPrune spares them on purpose), and that also deleted audit rows for
// artifacts living on other nodes, whose files this process cannot see at all.
//
// Deletion now happens where the plan is applied — the hourly agent, which
// prunes node, store and bucket through one implementation, or `backup prune`
// when an operator asks for it.
func (l *Local) Prune(ctx context.Context, policy Retention, dryRun bool) (PruneResult, error) {
	if policy.KeepDays == 0 && l.RetentionDays > 0 {
		policy.KeepDays = l.RetentionDays
	}
	if policy.KeepHours == 0 && policy.KeepDays == 0 {
		policy = DefaultRetention()
	}
	dir := l.ArchiveDir
	if dir == "" {
		dir = DefaultArchiveDir
	}
	res, err := PruneArtifacts(dir, policy, time.Now().UTC(), dryRun)
	if err != nil || dryRun || l.Store == nil || len(res.Deleted) == 0 {
		return res, err
	}
	if _, derr := DeleteRowsForArtifacts(ctx, l.Store, res.Deleted, policy.KeepDays); derr != nil {
		return res, derr
	}
	return res, nil
}

// DeleteRowsForArtifacts removes the audit rows of artifacts that were just
// deleted, matched by FILE NAME — never by age.
//
// Matching by age is what made the old implementation delete history belonging
// to other nodes: on the leader those rows describe files it cannot reach, so
// "older than the window" was the only test applied and every remote row older
// than three days disappeared from the console while its artifact survived.
//
// A row is removed only when every artifact it lists is in the deleted set, so
// a multi-volume run keeps its history as long as one of its archives remains.
func DeleteRowsForArtifacts(ctx context.Context, st *store.Store, deleted []string, keepDays int) (int, error) {
	if st == nil || len(deleted) == 0 {
		return 0, nil
	}
	gone := make(map[string]bool, len(deleted))
	for _, n := range deleted {
		gone[filepath.Base(strings.TrimSuffix(n, " (would delete)"))] = true
	}
	if keepDays < 0 {
		keepDays = 0
	}
	before := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour).Unix()
	rows, err := st.ListBackupsOlderThan(ctx, before)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, b := range rows {
		paths := splitArchivePaths(b.ArchivePaths)
		if len(paths) == 0 {
			continue
		}
		allGone := true
		for _, p := range paths {
			if p == "" || !gone[filepath.Base(p)] {
				allGone = false
				break
			}
		}
		if !allGone {
			continue
		}
		if err := st.DeleteBackup(ctx, b.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// List returns the most recent backup runs.
func (l *Local) List(ctx context.Context, limit int) ([]Run, error) {
	l.discover(ctx)
	rows, err := l.Store.ListBackups(ctx, limit)
	if err != nil {
		return nil, err
	}
	return runRows(rows), nil
}

// ListForStack returns the backup runs recorded for a single stack
// (including cluster-wide runs that cover every stack).
func (l *Local) ListForStack(ctx context.Context, stackName string) ([]Run, error) {
	l.discover(ctx)
	rows, err := l.Store.ListBackupsForStack(ctx, stackName)
	if err != nil {
		return nil, err
	}
	return runRows(rows), nil
}

func runRows(rows []*store.Backup) []Run {
	out := make([]Run, 0, len(rows))
	for _, b := range rows {
		r := Run{
			ID:           b.ID,
			Status:       b.Status,
			ArchivePaths: splitArchivePaths(b.ArchivePaths),
			ErrorMessage: b.ErrorMessage,
			StartedAt:    b.StartedAt,
		}
		if b.StackName.Valid {
			r.StackName = b.StackName.String
		}
		if b.Revision.Valid {
			r.Revision = b.Revision.Int64
		}
		if b.FinishedAt.Valid {
			r.FinishedAt = b.FinishedAt.Int64
		}
		out = append(out, r)
	}
	return out
}

func joinArchivePaths(paths []string) string {
	return strings.Join(paths, ",")
}

func splitArchivePaths(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
