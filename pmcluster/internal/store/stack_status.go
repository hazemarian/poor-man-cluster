package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// StackStatus is the reconcile loop's DB-backed snapshot of one stack's
// health. It is the ONLY source the public badge endpoints read — badges
// never query Docker live. The loop writes it every pass.
type StackStatus struct {
	StackName string
	Status    string            // healthy | in progress | degraded | error | unknown
	Services  map[string]string // per-service status, keyed by unqualified service name
	UpdatedAt int64
}

// SetStackStatus upserts one stack's status snapshot.
func (s *Store) SetStackStatus(ctx context.Context, st StackStatus) error {
	svcs, err := json.Marshal(st.Services)
	if err != nil {
		return fmt.Errorf("marshal service statuses: %w", err)
	}
	if st.Services == nil {
		svcs = []byte("{}")
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO stack_status (stack_name, status, services, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(stack_name) DO UPDATE SET
			status = excluded.status,
			services = excluded.services,
			updated_at = excluded.updated_at`,
		st.StackName, st.Status, string(svcs), st.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert stack status: %w", err)
	}
	return nil
}

// StackStatus returns the loop's latest snapshot for one stack.
// Returns ErrNotFound when the loop has not recorded one yet.
func (s *Store) StackStatus(ctx context.Context, stackName string) (*StackStatus, error) {
	var (
		status string
		svcs   string
		at     int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT status, services, updated_at FROM stack_status WHERE stack_name = ?`,
		stackName,
	).Scan(&status, &svcs, &at)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query stack status: %w", err)
	}
	services := map[string]string{}
	if err := json.Unmarshal([]byte(svcs), &services); err != nil {
		return nil, fmt.Errorf("decode service statuses: %w", err)
	}
	return &StackStatus{StackName: stackName, Status: status, Services: services, UpdatedAt: at}, nil
}

// ListStackStatuses returns every recorded status snapshot.
func (s *Store) ListStackStatuses(ctx context.Context) ([]*StackStatus, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT stack_name, status, services, updated_at FROM stack_status ORDER BY stack_name`)
	if err != nil {
		return nil, fmt.Errorf("query stack statuses: %w", err)
	}
	defer rows.Close()
	var out []*StackStatus
	for rows.Next() {
		var (
			name, status, svcs string
			at                 int64
		)
		if err := rows.Scan(&name, &status, &svcs, &at); err != nil {
			return nil, fmt.Errorf("scan stack status: %w", err)
		}
		services := map[string]string{}
		if err := json.Unmarshal([]byte(svcs), &services); err != nil {
			return nil, fmt.Errorf("decode service statuses: %w", err)
		}
		out = append(out, &StackStatus{StackName: name, Status: status, Services: services, UpdatedAt: at})
	}
	return out, rows.Err()
}

// DeleteStackStatus removes one stack's status snapshot (used by the reconcile
// loop to prune rows for stacks no longer in the store). Returns ErrNotFound
// when no snapshot exists.
func (s *Store) DeleteStackStatus(ctx context.Context, stackName string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM stack_status WHERE stack_name = ?`, stackName)
	if err != nil {
		return fmt.Errorf("delete stack status: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}
