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

func (a *Backups) Browse(ctx context.Context, id int64) (*backups.Run, []backups.FileEntry, error) {
	var out backupBrowseDTO
	if err := a.c.do(ctx, http.MethodGet, "/backups/"+strconv.FormatInt(id, 10)+"/files", nil, &out); err != nil {
		return nil, nil, err
	}
	return out.Run, out.Files, nil
}

func (a *Backups) Restore(ctx context.Context, id int64, destRoot string) (int, error) {
	var out backupRestoreDTO
	body := map[string]any{"dest_root": destRoot}
	if err := a.c.do(ctx, http.MethodPost, "/backups/"+strconv.FormatInt(id, 10)+"/restore", body, &out); err != nil {
		return 0, err
	}
	return out.Restored, nil
}

type backupListDTO struct {
	Backups []backups.Run `json:"backups"`
}

type backupBrowseDTO struct {
	Run   *backups.Run        `json:"run"`
	Files []backups.FileEntry `json:"files"`
}

type backupRestoreDTO struct {
	Restored int `json:"restored"`
}
