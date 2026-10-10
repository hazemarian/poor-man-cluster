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
	// RetentionDays prunes backup audit rows (and their archive files on
	// disk) older than this many days. Zero disables pruning.
	RetentionDays int
	// S3 is the offsite object-store destination. When configured, restore
	// falls back to fetching archives from it when the local copy is gone
	// (or --from-s3 is explicit). Empty disables the S3 fallback.
	S3 S3Config
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
	if l.ArchiveDir != "" {
		l.discoverLocal(ctx)
	}
	if l.S3.Configured() {
		l.discoverStore(ctx)
	}
}

// discoverLocal records scheduled offen archives found on disk that the audit
// table does not know about yet.
func (l *Local) discoverLocal(ctx context.Context) {
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
		// that already produced a row.
		if err := l.Store.RecordDiscoveredBackup(ctx, e.Name(), archivePath, info.ModTime().Unix()); err != nil {
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
func (l *Local) discoverStore(ctx context.Context) {
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
		if err := l.Store.RecordDiscoveredBackup(ctx, base, base, at); err != nil {
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

// prune removes backup audit rows older than RetentionDays together with
// their archive files on disk. It never fails the caller: rows that cannot
// be deleted are left in place and retried next time.
func (l *Local) prune(ctx context.Context) {
	if l.Store == nil || l.RetentionDays <= 0 {
		return
	}
	before := time.Now().Add(-time.Duration(l.RetentionDays) * 24 * time.Hour).Unix()
	rows, err := l.Store.ListBackupsOlderThan(ctx, before)
	if err != nil {
		return
	}
	for _, b := range rows {
		for _, p := range splitArchivePaths(b.ArchivePaths) {
			if p == "" {
				continue
			}
			_ = os.Remove(p)
		}
		_ = l.Store.DeleteBackup(ctx, b.ID)
	}
}

// List returns the most recent backup runs.
func (l *Local) List(ctx context.Context, limit int) ([]Run, error) {
	l.discover(ctx)
	l.prune(ctx)
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
	l.prune(ctx)
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
