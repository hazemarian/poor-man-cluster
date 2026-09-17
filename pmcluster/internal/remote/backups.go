package remote

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/backups"
)

// Backups is the HTTP adapter for backups.Service.
type Backups struct{ c *Client }

// NewBackups builds the remote backups adapter.
func NewBackups(c *Client) backups.Service { return &Backups{c: c} }

type backupCreatedDTO struct {
	ID           int64    `json:"id"`
	Status       string   `json:"status"`
	ArchivePaths []string `json:"archive_paths"`
}

func (a *Backups) Trigger(ctx context.Context, stackName string, revision int64) (int64, []string, error) {
	var out backupCreatedDTO
	if err := a.c.do(ctx, http.MethodPost, "/backups", nil, &out); err != nil {
		return 0, nil, err
	}
	return out.ID, out.ArchivePaths, nil
}

func (a *Backups) List(ctx context.Context, limit int) ([]backups.Run, error) {
	path := "/backups"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	var out backupListDTO
	if err := a.c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Backups, nil
}

func (a *Backups) ListForStack(ctx context.Context, stackName string) ([]backups.Run, error) {
	var out backupListDTO
	if err := a.c.do(ctx, http.MethodGet, "/stacks/"+url.PathEscape(stackName)+"/backups", nil, &out); err != nil {
		return nil, err
	}
	return out.Backups, nil
}

type backupListDTO struct {
	Backups []backups.Run `json:"backups"`
}
