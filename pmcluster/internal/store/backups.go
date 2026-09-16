package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Backup is one row in the backups audit table.
type Backup struct {
	ID           int64
	StackName    sql.NullString
	Revision     sql.NullInt64
	Status       string
	ArchivePaths string
	ErrorMessage string
	StartedAt    int64
	FinishedAt   sql.NullInt64
}

// ErrBackupNotFound is returned by GetBackup when no row matches.
var ErrBackupNotFound = errors.New("backup not found")

// CreateBackup inserts a new pending backup row and returns its id. The
// caller updates it later via FinishBackup once the offen exec returns.
func (s *Store) CreateBackup(ctx context.Context, stackName string, revision int64) (int64, error) {
	now := time.Now().Unix()
	var stack sql.NullString
	var rev sql.NullInt64
	if stackName != "" {
		stack = sql.NullString{String: stackName, Valid: true}
	}
	if revision != 0 {
		rev = sql.NullInt64{Int64: revision, Valid: true}
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO backups (stack_name, revision, status, started_at) VALUES (?, ?, 'pending', ?)`,
		stack, rev, now,
	)
	if err != nil {
		return 0, fmt.Errorf("insert backup: %w", err)
	}
	return res.LastInsertId()
}

// FinishBackup marks an in-flight backup as succeeded or failed and
// records the resulting archive list (or error message).
func (s *Store) FinishBackup(ctx context.Context, id int64, status, archivePaths, errorMessage string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE backups
		    SET status = ?, archive_paths = ?, error_message = ?, finished_at = ?
		  WHERE id = ?`,
		status, archivePaths, errorMessage, time.Now().Unix(), id,
	)
	if err != nil {
		return fmt.Errorf("update backup %d: %w", id, err)
	}
	return nil
}

// GetBackup fetches one backup row by id.
func (s *Store) GetBackup(ctx context.Context, id int64) (*Backup, error) {
	var b Backup
	err := s.db.QueryRowContext(ctx,
		`SELECT id, stack_name, revision, status, archive_paths, error_message, started_at, finished_at
		   FROM backups WHERE id = ?`, id,
	).Scan(&b.ID, &b.StackName, &b.Revision, &b.Status, &b.ArchivePaths, &b.ErrorMessage, &b.StartedAt, &b.FinishedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrBackupNotFound
		}
		return nil, fmt.Errorf("query backup: %w", err)
	}
	return &b, nil
}

// ListBackups returns the most recent backups, newest first. limit <= 0
// returns all rows.
func (s *Store) ListBackups(ctx context.Context, limit int) ([]*Backup, error) {
	q := `SELECT id, stack_name, revision, status, archive_paths, error_message, started_at, finished_at
	        FROM backups ORDER BY started_at DESC, id DESC`
	args := []any{}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query backups: %w", err)
	}
	defer rows.Close()
	var out []*Backup
	for rows.Next() {
		var b Backup
		if err := rows.Scan(&b.ID, &b.StackName, &b.Revision, &b.Status, &b.ArchivePaths, &b.ErrorMessage, &b.StartedAt, &b.FinishedAt); err != nil {
			return nil, fmt.Errorf("scan backup: %w", err)
		}
		out = append(out, &b)
	}
	return out, rows.Err()
}

// ListBackupsForStack returns backups associated with a specific stack,
// newest first.
func (s *Store) ListBackupsForStack(ctx context.Context, stackName string) ([]*Backup, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, stack_name, revision, status, archive_paths, error_message, started_at, finished_at
		   FROM backups WHERE stack_name = ? ORDER BY started_at DESC, id DESC`, stackName,
	)
	if err != nil {
		return nil, fmt.Errorf("query backups for stack: %w", err)
	}
	defer rows.Close()
	var out []*Backup
	for rows.Next() {
		var b Backup
		if err := rows.Scan(&b.ID, &b.StackName, &b.Revision, &b.Status, &b.ArchivePaths, &b.ErrorMessage, &b.StartedAt, &b.FinishedAt); err != nil {
			return nil, fmt.Errorf("scan backup: %w", err)
		}
		out = append(out, &b)
	}
	return out, rows.Err()
}
