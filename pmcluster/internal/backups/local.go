package backups

import (
	"context"
	"fmt"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// Local runs the on-demand volume backup pipeline: it records the run in
// the audit table, executes the backup trigger and finalizes the row with
// the resulting archive paths.
type Local struct {
	Store *store.Store
	Run   func(ctx context.Context) ([]string, error)
}

// NewLocal returns a Service bound to the given store and trigger. The
// trigger may be nil; List/ListForStack still work, Trigger returns an
// error describing that the trigger is not configured.
func NewLocal(st *store.Store, trigger func(ctx context.Context) ([]string, error)) Service {
	return &Local{Store: st, Run: trigger}
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
	paths, err := l.Run(ctx)
	if err != nil {
		_ = l.Store.FinishBackup(ctx, id, "failed", joinArchivePaths(paths), err.Error())
		RecordOutcome(ctx, KindOnDemand, StatusFailed)
		return id, paths, err
	}
	if err := l.Store.FinishBackup(ctx, id, "succeeded", joinArchivePaths(paths), ""); err != nil {
		return id, paths, fmt.Errorf("record backup finish: %w", err)
	}
	RecordOutcome(ctx, KindOnDemand, StatusSucceeded)
	return id, paths, nil
}

// List returns the most recent backup runs.
func (l *Local) List(ctx context.Context, limit int) ([]Run, error) {
	rows, err := l.Store.ListBackups(ctx, limit)
	if err != nil {
		return nil, err
	}
	return runRows(rows), nil
}

// ListForStack returns the backup runs recorded for a single stack.
func (l *Local) ListForStack(ctx context.Context, stackName string) ([]Run, error) {
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
