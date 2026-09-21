package settings

import (
	"context"
	"fmt"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// Local is the store-backed adapter for the settings port.
type Local struct {
	Store *store.Store
}

// NewLocal builds the local settings adapter.
func NewLocal(st *store.Store) *Local { return &Local{Store: st} }

// clusterSettingKeys is the fixed allowlist of editable settings surfaced to
// the console. TLS install state (mode/cert/key/acme) is deliberately
// excluded — those are managed by the setup wizard, not edited here.
var clusterSettingKeys = []string{
	cluster.SettingVolumeRoot(),
	cluster.SettingBackupAllNodes(),
	cluster.SettingBackupRetentionDays(),
	cluster.SettingSSOEnabled(),
	cluster.SettingSSOProvider(),
	cluster.SettingSSOClientID(),
	cluster.SettingSSOClientSecret(),
	cluster.SettingSSOGitHubOrg(),
	cluster.SettingSSOCookieExpire(),
	cluster.SettingEdgeLoginDisabled(),
	cluster.SettingDomain(),
	cluster.SettingOOEmail(),
	cluster.SettingTraefikAdminUser(),
}

// Get returns the current value of every known setting ("" when unset).
func (l *Local) Get(ctx context.Context) (Settings, error) {
	settings := make(Settings, len(clusterSettingKeys))
	for _, k := range clusterSettingKeys {
		settings[k] = l.Store.GetSettingDefault(ctx, k, "")
	}
	return settings, nil
}

// Update persists the given settings and returns the updated snapshot.
// Unknown keys are rejected atomically (nothing is written when any key is
// unknown).
func (l *Local) Update(ctx context.Context, values Settings) (Settings, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("settings: required")
	}
	allowed := map[string]bool{}
	for _, k := range clusterSettingKeys {
		allowed[k] = true
	}
	for k := range values {
		if !allowed[k] {
			return nil, fmt.Errorf("unknown setting: %s", k)
		}
	}
	for k, v := range values {
		if err := l.Store.SetSetting(ctx, k, v); err != nil {
			return nil, fmt.Errorf("set setting %s: %w", k, err)
		}
	}
	return l.Get(ctx)
}
