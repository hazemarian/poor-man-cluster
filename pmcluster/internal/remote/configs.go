package remote

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// Configs is the HTTP adapter for service.ConfigsService.
type Configs struct{ c *Client }

// NewConfigs builds the remote configs adapter.
func NewConfigs(c *Client) service.ConfigsService { return &Configs{c: c} }

type configRowDTO struct {
	ID        int64  `json:"id"`
	Scope     string `json:"scope"`
	Stack     string `json:"stack,omitempty"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Version   string `json:"version"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	Content   string `json:"content,omitempty"`
	Rendered  bool   `json:"rendered,omitempty"`
}

type configListDTO struct {
	Configs []configRowDTO `json:"configs"`
}

type configVersionDTO struct {
	ID        int64  `json:"id"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"created_at"`
}

type configVersionsDTO struct {
	Versions []configVersionDTO `json:"versions"`
}

type configHashDTO struct {
	Name         string `json:"name"`
	Hash         string `json:"hash"`
	RolledBackTo int64  `json:"rolled_back_to"`
}

type renderedConfigDTO struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type renderedConfigsDTO struct {
	Configs []renderedConfigDTO `json:"configs"`
}

func (a *Configs) Create(ctx context.Context, scope, stack, name, kind, content, version string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := a.c.do(ctx, http.MethodPost, "/configs", map[string]string{
		"scope":   scope,
		"stack":   stack,
		"name":    name,
		"kind":    kind,
		"content": content,
	}, &out)
	return out.ID, err
}

func (a *Configs) Get(ctx context.Context, name string) (*store.ConfigRow, error) {
	var dto configRowDTO
	if err := a.c.do(ctx, http.MethodGet, "/configs/"+url.PathEscape(name), nil, &dto); err != nil {
		return nil, err
	}
	return dto.row(), nil
}

func (a *Configs) List(ctx context.Context, scope, stack string) ([]*store.ConfigRow, error) {
	q := url.Values{}
	if scope != "" {
		q.Set("scope", scope)
	}
	if stack != "" {
		q.Set("stack", stack)
	}
	var out configListDTO
	if err := a.c.do(ctx, http.MethodGet, "/configs?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	rows := make([]*store.ConfigRow, 0, len(out.Configs))
	for i := range out.Configs {
		rows = append(rows, out.Configs[i].row())
	}
	return rows, nil
}

func (a *Configs) Update(ctx context.Context, name, content, version string) (string, error) {
	var out configHashDTO
	if err := a.c.do(ctx, http.MethodPut, "/configs/"+url.PathEscape(name), map[string]string{
		"content": content,
	}, &out); err != nil {
		return "", err
	}
	return out.Hash, nil
}

func (a *Configs) Rollback(ctx context.Context, name string, versionID int64) (string, error) {
	var out configHashDTO
	if err := a.c.do(ctx, http.MethodPost, "/configs/"+url.PathEscape(name)+"/rollback", map[string]int64{
		"version_id": versionID,
	}, &out); err != nil {
		return "", err
	}
	return out.Hash, nil
}

func (a *Configs) Delete(ctx context.Context, name string) error {
	return a.c.do(ctx, http.MethodDelete, "/configs/"+url.PathEscape(name), nil, nil)
}

func (a *Configs) ListVersions(ctx context.Context, name string) ([]*store.ConfigVersionRow, error) {
	var out configVersionsDTO
	if err := a.c.do(ctx, http.MethodGet, "/configs/"+url.PathEscape(name)+"/versions", nil, &out); err != nil {
		return nil, err
	}
	rows := make([]*store.ConfigVersionRow, 0, len(out.Versions))
	for _, v := range out.Versions {
		rows = append(rows, &store.ConfigVersionRow{ID: v.ID, Hash: v.Hash, CreatedAt: v.CreatedAt})
	}
	return rows, nil
}

// SetRendered is an internal snapshot operation performed by cluster update
// on the daemon; it has no API endpoint and is not supported remotely.
func (a *Configs) SetRendered(ctx context.Context, name, content string) error {
	return errors.New("set-rendered is an internal cluster-update operation and is not available over the remote API")
}

func (a *Configs) ListRendered(ctx context.Context) ([]*store.ConfigRow, error) {
	var out renderedConfigsDTO
	if err := a.c.do(ctx, http.MethodGet, "/cluster/rendered", nil, &out); err != nil {
		return nil, err
	}
	rows := make([]*store.ConfigRow, 0, len(out.Configs))
	for _, rc := range out.Configs {
		rows = append(rows, &store.ConfigRow{Name: rc.Name, RenderedContent: rc.Content})
	}
	return rows, nil
}

func (d configRowDTO) row() *store.ConfigRow {
	return &store.ConfigRow{
		ID:        d.ID,
		Scope:     d.Scope,
		Stack:     d.Stack,
		Name:      d.Name,
		Kind:      d.Kind,
		Version:   d.Version,
		Hash:      d.Hash,
		CreatedAt: d.CreatedAt,
		UpdatedAt: d.UpdatedAt,
		Content:   d.Content,
	}
}
