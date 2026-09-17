// Package impl provides local adapters implementing the service-layer ports.
package impl

import (
	"context"
	"fmt"
	"strings"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/backup"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// Backups runs the on-demand volume backup pipeline: it records the run in
// the audit table, executes the backup trigger and finalizes the row with
// the resulting archive paths.
type Backups struct {
	Store *store.Store
	Run   func(ctx context.Context) ([]string, error)
}

// NewBackups returns a BackupsService bound to the given store and trigger.
// The trigger may be nil; List/ListForStack still work, Trigger returns an
// error describing that the trigger is not configured.
func NewBackups(st *store.Store, trigger func(ctx context.Context) ([]string, error)) service.BackupsService {
	return &Backups{Store: st, Run: trigger}
}

// Trigger records the backup run, executes the trigger and finalizes the row.
func (b *Backups) Trigger(ctx context.Context, stackName string, revision int64) (int64, []string, error) {
	id, err := b.Store.CreateBackup(ctx, stackName, revision)
	if err != nil {
		return 0, nil, fmt.Errorf("record backup: %w", err)
	}
	if b.Run == nil {
		return id, nil, service.ErrBackupTriggerNotConfigured
	}
	paths, err := b.Run(ctx)
	if err != nil {
		_ = b.Store.FinishBackup(ctx, id, "failed", joinArchivePaths(paths), err.Error())
		backup.RecordOutcome(ctx, backup.KindOnDemand, backup.StatusFailed)
		return id, paths, err
	}
	if err := b.Store.FinishBackup(ctx, id, "succeeded", joinArchivePaths(paths), ""); err != nil {
		return id, paths, fmt.Errorf("record backup finish: %w", err)
	}
	backup.RecordOutcome(ctx, backup.KindOnDemand, backup.StatusSucceeded)
	return id, paths, nil
}

// List returns the most recent backup runs.
func (b *Backups) List(ctx context.Context, limit int) ([]*store.Backup, error) {
	return b.Store.ListBackups(ctx, limit)
}

// ListForStack returns the backup runs recorded for a single stack.
func (b *Backups) ListForStack(ctx context.Context, stackName string) ([]*store.Backup, error) {
	return b.Store.ListBackupsForStack(ctx, stackName)
}

func joinArchivePaths(paths []string) string {
	return strings.Join(paths, ",")
}
