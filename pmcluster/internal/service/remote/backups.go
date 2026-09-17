package remote

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"strconv"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// Backups is the HTTP adapter for service.BackupsService.
type Backups struct{ c *Client }

// NewBackups builds the remote backups adapter.
func NewBackups(c *Client) service.BackupsService { return &Backups{c: c} }

type backupDTO struct {
	ID           int64    `json:"id"`
	Status       string   `json:"status"`
	StackName    string   `json:"stack_name,omitempty"`
	Revision     int64    `json:"revision,omitempty"`
	ArchivePaths []string `json:"archive_paths"`
	ErrorMessage string   `json:"error_message,omitempty"`
	StartedAt    int64    `json:"started_at"`
	FinishedAt   int64    `json:"finished_at,omitempty"`
}

type backupListDTO struct {
	Backups []backupDTO `json:"backups"`
}

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

func (a *Backups) List(ctx context.Context, limit int) ([]*store.Backup, error) {
	var out backupListDTO
	path := "/backups"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	if err := a.c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return a.toRows(out.Backups), nil
}

func (a *Backups) ListForStack(ctx context.Context, stackName string) ([]*store.Backup, error) {
	var out backupListDTO
	if err := a.c.do(ctx, http.MethodGet, "/stacks/"+url.PathEscape(stackName)+"/backups", nil, &out); err != nil {
		return nil, err
	}
	return a.toRows(out.Backups), nil
}

func (a *Backups) toRows(dtos []backupDTO) []*store.Backup {
	rows := make([]*store.Backup, 0, len(dtos))
	for _, d := range dtos {
		var stack sql.NullString
		if d.StackName != "" {
			stack = sql.NullString{String: d.StackName, Valid: true}
		}
		var rev sql.NullInt64
		if d.Revision != 0 {
			rev = sql.NullInt64{Int64: d.Revision, Valid: true}
		}
		var finished sql.NullInt64
		if d.FinishedAt != 0 {
			finished = sql.NullInt64{Int64: d.FinishedAt, Valid: true}
		}
		rows = append(rows, &store.Backup{
			ID:           d.ID,
			StackName:    stack,
			Revision:     rev,
			Status:       d.Status,
			ArchivePaths: joinStrings(d.ArchivePaths),
			ErrorMessage: d.ErrorMessage,
			StartedAt:    d.StartedAt,
			FinishedAt:   finished,
		})
	}
	return rows
}
