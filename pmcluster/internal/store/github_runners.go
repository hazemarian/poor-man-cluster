package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GitHubRunner is one row of the console's runner view: what one node last
// observed about ITS runner. Rows are keyed by the runner name, which is unique
// per node, so several nodes sharing a store never overwrite each other.
//
// It is a cache of an observation, not the source of truth: the console merges
// it with GitHub's org runner list (see runners.Manager.Runners), because a
// registration is the thing a job actually arrives on.
type GitHubRunner struct {
	Name string
	// Org is the organisation the runner is registered in — the scope the
	// console shows in its first column.
	Org  string
	Node string
	// ContainerID is the local container id of the running runner ("" when this
	// node has no container: the row can describe a runner on another node).
	ContainerID string
	// Mode is "ephemeral" (the only mode the platform creates today) or
	// "persistent".
	Mode string
	// State is starting | idle | busy | offline | failed, derived from the
	// container and GitHub's runner list.
	State        string
	RegisteredAt int64
	// LastSeen is when the platform last observed this runner. A row whose
	// LastSeen stops advancing is stale: GitHub could not be reached, and the
	// state above is the last known one, not a fresh measurement.
	LastSeen int64
	// LastJobURL is the org's newest workflow run, captured when the runner left
	// "busy". Nullable in the schema; "" means never observed.
	LastJobURL string
	// Reason explains the row's state when it is not derivable from the container
	// and GitHub — today only a memory refusal ("1839 MB; a 2560m runner can OOM
	// this node"). Computed on every read, never persisted: it is a measurement,
	// and a stored copy could outlive the settings that produced it.
	Reason string
}

// ErrGitHubRunnerNotFound is returned by GitHubRunner for an unknown name.
var ErrGitHubRunnerNotFound = errors.New("github runner not found")

// PutGitHubRunner upserts one row. Each node writes only its own runner's row;
// there is deliberately no "replace the whole set" call, because a shared store
// would let one node delete another's row on every reconcile pass.
func (s *Store) PutGitHubRunner(ctx context.Context, r GitHubRunner) error {
	if r.Name == "" {
		return fmt.Errorf("runner row: empty name")
	}
	var lastJob any
	if r.LastJobURL != "" {
		lastJob = r.LastJobURL
	}
	if _, err := s.conn().ExecContext(ctx,
		`INSERT INTO github_runners
		   (name, org, node, container_id, mode, state, registered_at, last_seen, last_job_url)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		   org = excluded.org,
		   node = excluded.node,
		   container_id = excluded.container_id,
		   mode = excluded.mode,
		   state = excluded.state,
		   registered_at = excluded.registered_at,
		   last_seen = excluded.last_seen,
		   last_job_url = excluded.last_job_url`,
		r.Name, r.Org, r.Node, r.ContainerID, r.Mode, r.State, r.RegisteredAt, r.LastSeen, lastJob,
	); err != nil {
		return fmt.Errorf("upsert runner %s: %w", r.Name, err)
	}
	return nil
}

// ListGitHubRunners returns the current runners, ordered by org then name, and
// bounded so a runaway table cannot make the console's list unbounded.
func (s *Store) ListGitHubRunners(ctx context.Context, limit int) ([]GitHubRunner, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.conn().QueryContext(ctx,
		`SELECT name, org, node, container_id, mode, state, registered_at, last_seen, last_job_url
		   FROM github_runners ORDER BY org, name LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query runners: %w", err)
	}
	defer rows.Close()
	var out []GitHubRunner
	for rows.Next() {
		r, err := scanGitHubRunner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GitHubRunner returns one runner by name.
func (s *Store) GitHubRunner(ctx context.Context, name string) (*GitHubRunner, error) {
	row := s.conn().QueryRowContext(ctx,
		`SELECT name, org, node, container_id, mode, state, registered_at, last_seen, last_job_url
		   FROM github_runners WHERE name = ?`, name)
	r, err := scanGitHubRunner(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrGitHubRunnerNotFound
		}
		return nil, err
	}
	return &r, nil
}

// DeleteGitHubRunner removes one row. Idempotent: a kill of a runner whose row a
// concurrent reconcile already pruned is not an error.
func (s *Store) DeleteGitHubRunner(ctx context.Context, name string) error {
	if _, err := s.conn().ExecContext(ctx, `DELETE FROM github_runners WHERE name = ?`, name); err != nil {
		return fmt.Errorf("delete runner %s: %w", name, err)
	}
	return nil
}

// rowScanner is the shared surface of *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanGitHubRunner(row rowScanner) (GitHubRunner, error) {
	var r GitHubRunner
	var lastJob sql.NullString
	if err := row.Scan(&r.Name, &r.Org, &r.Node, &r.ContainerID, &r.Mode, &r.State,
		&r.RegisteredAt, &r.LastSeen, &lastJob); err != nil {
		return GitHubRunner{}, err
	}
	if lastJob.Valid {
		r.LastJobURL = lastJob.String
	}
	return r, nil
}
