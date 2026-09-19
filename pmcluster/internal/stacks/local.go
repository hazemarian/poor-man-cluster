package stacks

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// Local is the read-side adapter: it maps store rows into the stacks domain
// models. It is used wherever only the read side is needed — e.g. the daemon
// mounts it beside a Deployer that may come from elsewhere.
type Local struct {
	Store *store.Store
}

// Get returns a single stack's metadata, or store.ErrStackNotFound.
func (l Local) Get(ctx context.Context, name string) (*Stack, error) {
	row, err := l.Store.GetStack(ctx, name)
	if err != nil {
		return nil, err
	}
	s := stackFromRow(row)
	return &s, nil
}

// List returns every stack, ordered by name.
func (l Local) List(ctx context.Context) ([]Stack, error) {
	rows, err := l.Store.ListStacks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Stack, 0, len(rows))
	for _, row := range rows {
		out = append(out, stackFromRow(row))
	}
	return out, nil
}

// Revisions returns up to limit most recent revisions, newest first.
func (l Local) Revisions(ctx context.Context, name string, limit int) ([]Revision, error) {
	rows, err := l.Store.ListRevisions(ctx, name, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Revision, 0, len(rows))
	for _, row := range rows {
		out = append(out, revisionFromRow(row))
	}
	return out, nil
}

// The engine also satisfies Reader (backed by its own store) so a single
// *Service can serve both the write and read sides, which is what the CLI
// deploy/stack commands need.

// Get returns a single stack via the engine's store.
func (s *Service) Get(ctx context.Context, name string) (*Stack, error) {
	return Local{Store: s.Store}.Get(ctx, name)
}

// List returns every stack via the engine's store.
func (s *Service) List(ctx context.Context) ([]Stack, error) {
	return Local{Store: s.Store}.List(ctx)
}

// Revisions returns a stack's revision history via the engine's store.
func (s *Service) Revisions(ctx context.Context, name string, limit int) ([]Revision, error) {
	return Local{Store: s.Store}.Revisions(ctx, name, limit)
}

func stackFromRow(row *store.Stack) Stack {
	return Stack{
		Name:            row.Name,
		CurrentRevision: row.CurrentRevision,
		RepoURL:         row.RepoURL.String,
		SourceFile:      row.SourceFile,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func revisionFromRow(row *store.StackRevision) Revision {
	return Revision{
		StackName:    row.StackName,
		Revision:     row.Revision,
		SourceYAML:   row.SourceYAML,
		RenderedYAML: row.RenderedYAML,
		PayloadJSON:  row.PayloadJSON.String,
		CreatedAt:    row.CreatedAt,
	}
}
