// Package backups owns the on-demand volume backup pipeline: the audit
// records, the offen trigger and the HTTP surface.
package backups

import (
	"context"
	"errors"
)

// ErrTriggerNotConfigured is returned by Service.Trigger when no backup
// trigger is wired. The daemon always wires one; consumers that only list
// backups can run without one.
var ErrTriggerNotConfigured = errors.New("backup trigger not configured")

// Run is one recorded backup attempt. Empty StackName / Revision / FinishedAt
// mean the run was not tied to a stack deploy or has not finished yet.
type Run struct {
	ID           int64    `json:"id"`
	Status       string   `json:"status"`
	StackName    string   `json:"stack_name,omitempty"`
	Revision     int64    `json:"revision,omitempty"`
	ArchivePaths []string `json:"archive_paths,omitempty"`
	ErrorMessage string   `json:"error_message,omitempty"`
	StartedAt    int64    `json:"started_at"`
	FinishedAt   int64    `json:"finished_at,omitempty"`
}

// Service triggers and lists backups of stack volumes.
type Service interface {
	// Trigger records the backup run, executes the backup and finalizes the
	// audit row with the resulting archive paths.
	Trigger(ctx context.Context, stackName string, revision int64) (id int64, paths []string, err error)
	// List returns the most recent backup runs.
	List(ctx context.Context, limit int) ([]Run, error)
	// ListForStack returns the backup runs recorded for a single stack.
	ListForStack(ctx context.Context, stackName string) ([]Run, error)
	// Browse lists the archive contents of one backup run.
	Browse(ctx context.Context, id int64) (*Run, []FileEntry, error)
	// Restore extracts every archive of a successful stack-scoped backup
	// run back under destRoot.
	Restore(ctx context.Context, id int64, destRoot string) (restoredFiles int, err error)
}
