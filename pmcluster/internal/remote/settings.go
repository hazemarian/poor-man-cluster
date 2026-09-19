package remote

import (
	"context"
	"net/http"
)

// ClusterSettings is the HTTP adapter for the editable cluster settings
// surface (GET/PUT /api/cluster/settings). It is a plain client — there is no
// local/service port, since the console and CLI only ever read/write these
// keys over HTTP.
type ClusterSettings struct{ c *Client }

// NewClusterSettings builds the remote cluster-settings adapter.
func NewClusterSettings(c *Client) *ClusterSettings { return &ClusterSettings{c: c} }

type clusterSettingsDTO struct {
	Settings map[string]string `json:"settings"`
}

// GetClusterSettings returns the current value of every editable cluster
// setting.
func (s *ClusterSettings) GetClusterSettings(ctx context.Context) (map[string]string, error) {
	var out clusterSettingsDTO
	if err := s.c.do(ctx, http.MethodGet, "/cluster/settings", nil, &out); err != nil {
		return nil, err
	}
	return out.Settings, nil
}

// UpdateClusterSettings persists a set of cluster settings and returns the
// updated map. The daemon rejects unknown keys with 400.
func (s *ClusterSettings) UpdateClusterSettings(ctx context.Context, settings map[string]string) (map[string]string, error) {
	var out clusterSettingsDTO
	if err := s.c.do(ctx, http.MethodPut, "/cluster/settings", clusterSettingsDTO{Settings: settings}, &out); err != nil {
		return nil, err
	}
	return out.Settings, nil
}
