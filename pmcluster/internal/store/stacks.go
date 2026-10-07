package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Stack metadata; compose contents live in StackRevision rows keyed by
// unix-timestamp revision id.
type Stack struct {
	Name            string
	CurrentRevision int64
	RepoURL         sql.NullString
	// SourceFile is the manifest path inside the repo (e.g. deploy/test-lms.yaml)
	// that produced the current revision; empty when deployed without provenance.
	SourceFile string
	// LastError holds the deploy/apply outcome history for this stack as a
	// JSON array of StackErrorEntry, newest first ("" when the last deploy
	// succeeded and no prior failure is retained). Fire-and-forget deploys
	// apply in the background, so failures surface here instead of in the
	// HTTP response.
	LastError string
	CreatedAt int64
	UpdatedAt int64
}

// StackRevision records one deployed version of a stack: the source DSL, the
// rendered compose, and the checksum used to skip no-op syncs.
type StackRevision struct {
	StackName    string
	Revision     int64
	SourceYAML   string
	RenderedYAML string
	// RenderedHash is sha256(rendered_yaml), computed at deploy time. A
	// re-translated manifest that hashes identically to the latest revision
	// is a no-op — no new revision and no Docker call.
	RenderedHash string
	// SourceFile is the manifest path inside the source repo for THIS revision.
	SourceFile  string
	PayloadJSON sql.NullString
	CreatedAt   int64
}

// ErrStackNotFound indicates a stack row does not exist.
var ErrStackNotFound = errors.New("stack not found")

// ErrRevisionNotFound indicates a revision row does not exist.
var ErrRevisionNotFound = errors.New("revision not found")

// StackErrorEntry is one deploy/apply outcome in a stack's error history.
// The stacks.last_error column holds a JSON array of these, newest first;
// an entry with an empty Error records a successful apply (which clears the
// surfaced "latest error" while the history is retained).
type StackErrorEntry struct {
	Revision  int64  `json:"revision"`
	Error     string `json:"error"`
	CreatedAt int64  `json:"created_at"`
}

// StackErrorHistoryLimit caps how many outcomes the last_error column keeps.
const StackErrorHistoryLimit = 20

// ParseStackErrors decodes the last_error column (a JSON array of
// StackErrorEntry, newest first) into a slice. Empty or malformed content
// yields nil rather than an error so callers never fail on a legacy/blank row.
func ParseStackErrors(raw string) []StackErrorEntry {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil
	}
	var out []StackErrorEntry
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

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
		   (stack_name, revision, source_yaml, rendered_yaml, rendered_hash, source_file, payload_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rev.StackName, rev.Revision, rev.SourceYAML, rev.RenderedYAML, rev.RenderedHash,
		rev.SourceFile, nullableString(rev.PayloadJSON.String, rev.PayloadJSON.Valid), now,
	); err != nil {
		if !isForeignKeyViolation(err) {
			return fmt.Errorf("insert revision: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stacks (name, current_revision, repo_url, source_file, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			rev.StackName, rev.Revision, nullableString(repoURL, repoURL != ""), rev.SourceFile, now, now,
		); err != nil {
			return fmt.Errorf("insert stack: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stack_revisions
			   (stack_name, revision, source_yaml, rendered_yaml, rendered_hash, source_file, payload_json, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			rev.StackName, rev.Revision, rev.SourceYAML, rev.RenderedYAML, rev.RenderedHash,
			rev.SourceFile, nullableString(rev.PayloadJSON.String, rev.PayloadJSON.Valid), now,
		); err != nil {
			return fmt.Errorf("insert revision (after parent): %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		return nil
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE stacks SET current_revision = ?, updated_at = ?, repo_url = COALESCE(NULLIF(?, ''), repo_url),
		 source_file = COALESCE(NULLIF(?, ''), source_file)
		 WHERE name = ?`,
		rev.Revision, now, repoURL, rev.SourceFile, rev.StackName,
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
			`INSERT INTO stacks (name, current_revision, repo_url, source_file, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			rev.StackName, rev.Revision, nullableString(repoURL, repoURL != ""), rev.SourceFile, now, now,
		); err != nil {
			return fmt.Errorf("re-insert stack: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// SetRevisionRenderedHash stamps the rendered_hash column of an existing
// revision. The deploy pipeline records a revision with an EMPTY hash up
// front, then sets the hash only after the swarm apply succeeds — so a failed
// deploy leaves the hash empty and the next Sync sees a mismatch and retries
// (BUG-018). Returns ErrRevisionNotFound when no such revision exists.
func (s *Store) SetRevisionRenderedHash(ctx context.Context, stackName string, revision int64, hash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE stack_revisions SET rendered_hash = ? WHERE stack_name = ? AND revision = ?`,
		hash, stackName, revision,
	)
	if err != nil {
		return fmt.Errorf("update revision rendered_hash: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrRevisionNotFound
	}
	return nil
}

func (s *Store) GetStack(ctx context.Context, name string) (*Stack, error) {
	var st Stack
	err := s.db.QueryRowContext(ctx,
		`SELECT name, current_revision, repo_url, source_file, last_error, created_at, updated_at FROM stacks WHERE name = ?`, name,
	).Scan(&st.Name, &st.CurrentRevision, &st.RepoURL, &st.SourceFile, &st.LastError, &st.CreatedAt, &st.UpdatedAt)
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
		`SELECT name, current_revision, repo_url, source_file, last_error, created_at, updated_at FROM stacks ORDER BY name`,
	)
	if err != nil {
		return nil, fmt.Errorf("query stacks: %w", err)
	}
	defer rows.Close()
	var out []*Stack
	for rows.Next() {
		var st Stack
		if err := rows.Scan(&st.Name, &st.CurrentRevision, &st.RepoURL, &st.SourceFile, &st.LastError, &st.CreatedAt, &st.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan stack: %w", err)
		}
		out = append(out, &st)
	}
	return out, rows.Err()
}

// SetStackLastError records the most recent deploy/apply error for a stack
// ("" clears it). ErrStackNotFound when the stack does not exist.
func (s *Store) SetStackLastError(ctx context.Context, name, lastError string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE stacks SET last_error = ?, updated_at = ? WHERE name = ?`,
		lastError, time.Now().Unix(), name,
	)
	if err != nil {
		return fmt.Errorf("update stack last_error: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrStackNotFound
	}
	return nil
}

// RecordStackError records ONE deploy/apply FAILURE in the stack's error
// history, stored as a JSON array in the stacks.last_error column (newest
// first, capped at StackErrorHistoryLimit). Only failures are recorded — a
// clean apply writes nothing, so the history stays pure signal and the
// revision join decides what to display. ErrStackNotFound when the stack does
// not exist.
func (s *Store) RecordStackError(ctx context.Context, stackName string, revision int64, errMsg string) (int64, error) {
	if errMsg == "" {
		// Clean apply — no history entry, no write.
		return time.Now().Unix(), nil
	}
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT last_error FROM stacks WHERE name = ?`, stackName).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrStackNotFound
		}
		return 0, fmt.Errorf("read stack last_error: %w", err)
	}
	entries := ParseStackErrors(raw)
	entries = append([]StackErrorEntry{{Revision: revision, Error: errMsg, CreatedAt: time.Now().Unix()}}, entries...)
	if len(entries) > StackErrorHistoryLimit {
		entries = entries[:StackErrorHistoryLimit]
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return 0, fmt.Errorf("marshal stack error history: %w", err)
	}
	if err := s.SetStackLastError(ctx, stackName, string(b)); err != nil {
		return 0, err
	}
	return time.Now().Unix(), nil
}

// ListStackErrors decodes the newest `limit` outcomes from the stack's
// last_error history column, newest first. limit <= 0 returns every kept row.
func (s *Store) ListStackErrors(ctx context.Context, stackName string, limit int) ([]StackErrorEntry, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT last_error FROM stacks WHERE name = ?`, stackName).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrStackNotFound
		}
		return nil, fmt.Errorf("read stack last_error: %w", err)
	}
	entries := ParseStackErrors(raw)
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
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
		`SELECT stack_name, revision, source_yaml, rendered_yaml, rendered_hash, source_file, payload_json, created_at
		 FROM stack_revisions WHERE stack_name = ? AND revision = ?`,
		stackName, revision,
	).Scan(&r.StackName, &r.Revision, &r.SourceYAML, &r.RenderedYAML, &r.RenderedHash, &r.SourceFile, &r.PayloadJSON, &r.CreatedAt)
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
			`SELECT stack_name, revision, source_yaml, rendered_yaml, rendered_hash, source_file, payload_json, created_at
			 FROM stack_revisions WHERE stack_name = ? ORDER BY revision DESC LIMIT ?`,
			stackName, limit,
		)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT stack_name, revision, source_yaml, rendered_yaml, rendered_hash, source_file, payload_json, created_at
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
		if err := rows.Scan(&r.StackName, &r.Revision, &r.SourceYAML, &r.RenderedYAML, &r.RenderedHash, &r.SourceFile, &r.PayloadJSON, &r.CreatedAt); err != nil {
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
