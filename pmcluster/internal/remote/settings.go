package remote

import (
	"context"
	"net/http"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/settings"
)

// ClusterSettings is the HTTP adapter for the editable cluster settings
// surface (GET/PUT /api/cluster/settings). It implements settings.Service.
type ClusterSettings struct{ c *Client }

// NewClusterSettings builds the remote cluster-settings adapter.
func NewClusterSettings(c *Client) *ClusterSettings { return &ClusterSettings{c: c} }

// GetClusterSettings returns the current value of every editable cluster
// setting.
func (s *ClusterSettings) GetClusterSettings(ctx context.Context) (settings.Settings, error) {
	var out clusterSettingsDTO
	if err := s.c.do(ctx, http.MethodGet, "/cluster/settings", nil, &out); err != nil {
		return nil, err
	}
	return out.Settings, nil
}

// Get returns the current value of every editable cluster setting.
func (s *ClusterSettings) Get(ctx context.Context) (settings.Settings, error) {
	return s.GetClusterSettings(ctx)
}

// UpdateClusterSettings persists a set of cluster settings and returns the
// updated map. The daemon rejects unknown keys with 400.
func (s *ClusterSettings) UpdateClusterSettings(ctx context.Context, values settings.Settings) (settings.Settings, error) {
	var out clusterSettingsDTO
	if err := s.c.do(ctx, http.MethodPut, "/cluster/settings", clusterSettingsDTO{Settings: values}, &out); err != nil {
		return nil, err
	}
	return out.Settings, nil
}

// Update persists a set of cluster settings and returns the updated map.
func (s *ClusterSettings) Update(ctx context.Context, values settings.Settings) (settings.Settings, error) {
	return s.UpdateClusterSettings(ctx, values)
}

type clusterSettingsDTO struct {
	Settings settings.Settings `json:"settings"`
}
