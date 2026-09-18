package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Stack metadata; compose contents live in StackRevision rows keyed by
// unix-timestamp revision id.
type Stack struct {
	Name            string
	CurrentRevision int64
	RepoURL         sql.NullString
	CreatedAt       int64
	UpdatedAt       int64
}

type StackRevision struct {
	StackName    string
	Revision     int64
	SourceYAML   string
	RenderedYAML string
	// RenderedHash is sha256(rendered_yaml), computed at deploy time. A
	// re-translated manifest that hashes identically to the latest revision
	// is a no-op — no new revision and no Docker call.
	RenderedHash string
	PayloadJSON  sql.NullString
	CreatedAt    int64
}

var ErrStackNotFound = errors.New("stack not found")
var ErrRevisionNotFound = errors.New("revision not found")

// RecordDeploy atomically inserts the new revision and upserts the stack
// row's current_revision pointer.
func (s *Store) RecordDeploy(ctx context.Context, rev *StackRevision, repoURL string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO stack_revisions
		   (stack_name, revision, source_yaml, rendered_yaml, rendered_hash, payload_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rev.StackName, rev.Revision, rev.SourceYAML, rev.RenderedYAML, rev.RenderedHash,
		nullableString(rev.PayloadJSON.String, rev.PayloadJSON.Valid), now,
	); err != nil {
		if !isForeignKeyViolation(err) {
			return fmt.Errorf("insert revision: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stacks (name, current_revision, repo_url, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?)`,
			rev.StackName, rev.Revision, nullableString(repoURL, repoURL != ""), now, now,
		); err != nil {
			return fmt.Errorf("insert stack: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stack_revisions
			   (stack_name, revision, source_yaml, rendered_yaml, rendered_hash, payload_json, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			rev.StackName, rev.Revision, rev.SourceYAML, rev.RenderedYAML, rev.RenderedHash,
			nullableString(rev.PayloadJSON.String, rev.PayloadJSON.Valid), now,
		); err != nil {
			return fmt.Errorf("insert revision (after parent): %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		return nil
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE stacks SET current_revision = ?, updated_at = ?, repo_url = COALESCE(NULLIF(?, ''), repo_url)
		 WHERE name = ?`,
		rev.Revision, now, repoURL, rev.StackName,
	)
	if err != nil {
		return fmt.Errorf("update stack: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stacks (name, current_revision, repo_url, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?)`,
			rev.StackName, rev.Revision, nullableString(repoURL, repoURL != ""), now, now,
		); err != nil {
			return fmt.Errorf("re-insert stack: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (s *Store) GetStack(ctx context.Context, name string) (*Stack, error) {
	var st Stack
	err := s.db.QueryRowContext(ctx,
		`SELECT name, current_revision, repo_url, created_at, updated_at FROM stacks WHERE name = ?`, name,
	).Scan(&st.Name, &st.CurrentRevision, &st.RepoURL, &st.CreatedAt, &st.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrStackNotFound
		}
		return nil, fmt.Errorf("query stack: %w", err)
	}
	return &st, nil
}

func (s *Store) ListStacks(ctx context.Context) ([]*Stack, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, current_revision, repo_url, created_at, updated_at FROM stacks ORDER BY name`,
	)
	if err != nil {
		return nil, fmt.Errorf("query stacks: %w", err)
	}
	defer rows.Close()
	var out []*Stack
	for rows.Next() {
		var st Stack
		if err := rows.Scan(&st.Name, &st.CurrentRevision, &st.RepoURL, &st.CreatedAt, &st.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan stack: %w", err)
		}
		out = append(out, &st)
	}
	return out, rows.Err()
}

// DeleteStack removes the stack row, its revisions (via the ON DELETE CASCADE
// foreign key) and every service-scope config + secret the stack owns (a stack
// owns those rows — deleting a stack must not leave orphaned configs/secrets).
// Returns ErrStackNotFound when no such stack exists.
func (s *Store) DeleteStack(ctx context.Context, name string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM configs WHERE stack = ?`, name); err != nil {
		return fmt.Errorf("delete stack configs: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE stack = ?`, name); err != nil {
		return fmt.Errorf("delete stack secrets: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM stacks WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete stack %s: %w", name, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrStackNotFound
	}
	return nil
}

// NextFreeRevision returns the smallest unused revision id ≥ candidate.
// Lets callers pass time.Now().Unix() and survive sub-second collisions
// from back-to-back deploys.
func (s *Store) NextFreeRevision(ctx context.Context, stackName string, candidate int64) (int64, error) {
	const maxAttempts = 10000
	for i := int64(0); i < maxAttempts; i++ {
		r := candidate + i
		var dummy int64
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM stack_revisions WHERE stack_name = ? AND revision = ?`,
			stackName, r,
		).Scan(&dummy)
		if errors.Is(err, sql.ErrNoRows) {
			return r, nil
		}
		if err != nil {
			return 0, fmt.Errorf("check revision %d: %w", r, err)
		}
	}
	return 0, fmt.Errorf("could not find a free revision for %s near %d after %d attempts", stackName, candidate, maxAttempts)
}

func (s *Store) GetRevision(ctx context.Context, stackName string, revision int64) (*StackRevision, error) {
	var r StackRevision
	err := s.db.QueryRowContext(ctx,
		`SELECT stack_name, revision, source_yaml, rendered_yaml, rendered_hash, payload_json, created_at
		 FROM stack_revisions WHERE stack_name = ? AND revision = ?`,
		stackName, revision,
	).Scan(&r.StackName, &r.Revision, &r.SourceYAML, &r.RenderedYAML, &r.RenderedHash, &r.PayloadJSON, &r.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRevisionNotFound
		}
		return nil, fmt.Errorf("query revision: %w", err)
	}
	return &r, nil
}

// ListRevisions returns up to limit most recent revisions, newest first.
// limit ≤ 0 means all.
func (s *Store) ListRevisions(ctx context.Context, stackName string, limit int) ([]*StackRevision, error) {
	var rows *sql.Rows
	var err error
	if limit > 0 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT stack_name, revision, source_yaml, rendered_yaml, rendered_hash, payload_json, created_at
			 FROM stack_revisions WHERE stack_name = ? ORDER BY revision DESC LIMIT ?`,
			stackName, limit,
		)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT stack_name, revision, source_yaml, rendered_yaml, rendered_hash, payload_json, created_at
			 FROM stack_revisions WHERE stack_name = ? ORDER BY revision DESC`,
			stackName,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("query revisions: %w", err)
	}
	defer rows.Close()
	var out []*StackRevision
	for rows.Next() {
		var r StackRevision
		if err := rows.Scan(&r.StackName, &r.Revision, &r.SourceYAML, &r.RenderedYAML, &r.RenderedHash, &r.PayloadJSON, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan revision: %w", err)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

func nullableString(s string, valid bool) any {
	if !valid {
		return nil
	}
	return s
}

// isForeignKeyViolation matches modernc.org/sqlite's FK error string.
func isForeignKeyViolation(err error) bool {
	return err != nil && containsAny(err.Error(),
		"FOREIGN KEY constraint failed",
		"foreign key constraint failed",
	)
}
