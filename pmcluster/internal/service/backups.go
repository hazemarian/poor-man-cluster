package service

import (
	"context"
	"errors"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// ErrBackupTriggerNotConfigured is returned by BackupsService.Trigger when no
// backup trigger is wired. The daemon always wires one; consumers that only
// list backups can run without one.
var ErrBackupTriggerNotConfigured = errors.New("backup trigger not configured")

// BackupsService triggers and lists backups of stack volumes.
type BackupsService interface {
	// Trigger records the backup run, executes the backup and finalizes the
	// audit row with the resulting archive paths.
	Trigger(ctx context.Context, stackName string, revision int64) (id int64, paths []string, err error)
	// List returns the most recent backup runs.
	List(ctx context.Context, limit int) ([]*store.Backup, error)
	// ListForStack returns the backup runs recorded for a single stack.
	ListForStack(ctx context.Context, stackName string) ([]*store.Backup, error)
}
