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

// discover records scheduled offen archives found on disk that the audit
// table does not know about yet. It never fails the caller: a missing or
// unreadable archive dir simply records nothing.
func (l *Local) discover(ctx context.Context) {
	if l.Store == nil || l.ArchiveDir == "" {
		return
	}
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

// List returns the most recent backup runs.
func (l *Local) List(ctx context.Context, limit int) ([]Run, error) {
	l.discover(ctx)
	rows, err := l.Store.ListBackups(ctx, limit)
	if err != nil {
		return nil, err
	}
	return runRows(rows), nil
}

// ListForStack returns the backup runs recorded for a single stack.
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
