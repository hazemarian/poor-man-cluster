package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// StackFailover is the marker recorded when the control loop automatically
// relocates a stateful stack off a failed storage node (restoring the latest
// offsite backup on a healthy node). It is cleared when the stack is moved
// back to its original node, or acknowledged (Acked) to accept the new node.
type StackFailover struct {
	StackName string
	FromNode  string
	ToNode    string
	At        int64
	Acked     bool
}

// SetStackFailover upserts the failover marker for a stack. A fresh automatic
// failover always starts unacknowledged.
func (s *Store) SetStackFailover(ctx context.Context, f StackFailover) error {
	acked := 0
	if f.Acked {
		acked = 1
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO stack_failover (stack_name, from_node, to_node, at, acked)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(stack_name) DO UPDATE SET
			from_node = excluded.from_node,
			to_node = excluded.to_node,
			at = excluded.at,
			acked = excluded.acked`,
		f.StackName, f.FromNode, f.ToNode, f.At, acked); err != nil {
		return fmt.Errorf("upsert stack failover: %w", err)
	}
	return nil
}

// StackFailover returns the failover marker for a stack, or ErrNotFound when
// the stack has not been failed over.
func (s *Store) StackFailover(ctx context.Context, stackName string) (*StackFailover, error) {
	var (
		f     StackFailover
		acked int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT stack_name, from_node, to_node, at, acked FROM stack_failover WHERE stack_name = ?`,
		stackName,
	).Scan(&f.StackName, &f.FromNode, &f.ToNode, &f.At, &acked)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query stack failover: %w", err)
	}
	f.Acked = acked != 0
	return &f, nil
}

// AckStackFailover marks the stack's failover as acknowledged — the operator
// accepts the app on its current node as healthy. Returns ErrNotFound when no
// marker exists.
func (s *Store) AckStackFailover(ctx context.Context, stackName string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE stack_failover SET acked = 1 WHERE stack_name = ?`, stackName)
	if err != nil {
		return fmt.Errorf("ack stack failover: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearStackFailover removes the marker (used when a stack is moved back to its
// original node). Returns ErrNotFound when no marker exists.
func (s *Store) ClearStackFailover(ctx context.Context, stackName string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM stack_failover WHERE stack_name = ?`, stackName)
	if err != nil {
		return fmt.Errorf("clear stack failover: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListStackFailovers returns every recorded failover marker.
func (s *Store) ListStackFailovers(ctx context.Context) ([]*StackFailover, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT stack_name, from_node, to_node, at, acked FROM stack_failover ORDER BY stack_name`)
	if err != nil {
		return nil, fmt.Errorf("query stack failovers: %w", err)
	}
	defer rows.Close()
	var out []*StackFailover
	for rows.Next() {
		var (
			f     StackFailover
			acked int
		)
		if err := rows.Scan(&f.StackName, &f.FromNode, &f.ToNode, &f.At, &acked); err != nil {
			return nil, fmt.Errorf("scan stack failover: %w", err)
		}
		f.Acked = acked != 0
		out = append(out, &f)
	}
	return out, rows.Err()
}
