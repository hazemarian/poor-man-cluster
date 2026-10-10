package remote

import (
	"context"
	"net/http"
	"net/url"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/configs"
)

// Configs is the HTTP adapter for configs.Service.
type Configs struct{ c *Client }

// NewConfigs builds the remote configs adapter.
func NewConfigs(c *Client) configs.Service { return &Configs{c: c} }

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

// Create stores a new config via POST /configs and returns its row ID.
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

// Get returns one config row via GET /configs/{name}.
func (a *Configs) Get(ctx context.Context, name string) (*configs.Config, error) {
	var dto configRowDTO
	if err := a.c.do(ctx, http.MethodGet, "/configs/"+url.PathEscape(name), nil, &dto); err != nil {
		return nil, err
	}
	return dto.model(), nil
}

// List returns the config rows matching the optional scope and stack
// filters via GET /configs.
func (a *Configs) List(ctx context.Context, scope, stack string) ([]configs.Config, error) {
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
	rows := make([]configs.Config, 0, len(out.Configs))
	for i := range out.Configs {
		rows = append(rows, *out.Configs[i].model())
	}
	return rows, nil
}

// Update writes new content for a config via PUT /configs/{name} and returns
// the new content hash.
func (a *Configs) Update(ctx context.Context, name, content, version string) (string, error) {
	var out configHashDTO
	if err := a.c.do(ctx, http.MethodPut, "/configs/"+url.PathEscape(name), map[string]string{
		"content": content,
	}, &out); err != nil {
		return "", err
	}
	return out.Hash, nil
}

// Rollback restores a stored config version via POST /configs/{name}/rollback
// and returns the restored content hash.
func (a *Configs) Rollback(ctx context.Context, name string, versionID int64) (string, error) {
	var out configHashDTO
	if err := a.c.do(ctx, http.MethodPost, "/configs/"+url.PathEscape(name)+"/rollback", map[string]int64{
		"version_id": versionID,
	}, &out); err != nil {
		return "", err
	}
	return out.Hash, nil
}

// Delete removes a config row via DELETE /configs/{name}.
func (a *Configs) Delete(ctx context.Context, name string) error {
	return a.c.do(ctx, http.MethodDelete, "/configs/"+url.PathEscape(name), nil, nil)
}

// Retag reassigns the config's scope and stack on the remote daemon.
func (a *Configs) Retag(ctx context.Context, name, scope, stack string) error {
	return a.c.do(ctx, http.MethodPut, "/configs/"+url.PathEscape(name)+"/stack", map[string]string{
		"scope": scope,
		"stack": stack,
	}, nil)
}

// ListVersions returns a config's stored versions via GET
// /configs/{name}/versions.
func (a *Configs) ListVersions(ctx context.Context, name string) ([]configs.ConfigVersion, error) {
	var out configVersionsDTO
	if err := a.c.do(ctx, http.MethodGet, "/configs/"+url.PathEscape(name)+"/versions", nil, &out); err != nil {
		return nil, err
	}
	rows := make([]configs.ConfigVersion, 0, len(out.Versions))
	for _, v := range out.Versions {
		rows = append(rows, configs.ConfigVersion{ID: v.ID, Hash: v.Hash, CreatedAt: v.CreatedAt})
	}
	return rows, nil
}

// ListRendered returns the rendered-snapshot rows via GET /cluster/rendered.
func (a *Configs) ListRendered(ctx context.Context) ([]configs.Config, error) {
	var out renderedConfigsDTO
	if err := a.c.do(ctx, http.MethodGet, "/cluster/rendered", nil, &out); err != nil {
		return nil, err
	}
	rows := make([]configs.Config, 0, len(out.Configs))
	for _, rc := range out.Configs {
		rows = append(rows, configs.Config{Name: rc.Name, Rendered: rc.Content})
	}
	return rows, nil
}

func (d configRowDTO) model() *configs.Config {
	return &configs.Config{
		ID:        d.ID,
		Scope:     d.Scope,
		Stack:     d.Stack,
		Name:      d.Name,
		Kind:      d.Kind,
		Version:   d.Version,
		Hash:      d.Hash,
		Content:   d.Content,
		CreatedAt: d.CreatedAt,
		UpdatedAt: d.UpdatedAt,
	}
}
