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

	// ApplyLogLevel, when set, re-levels the running daemon's logger when a
	// settings update changes log_level. Wired by the daemon composition root
	// (logger.SetLevel); nil keeps the settings domain pure (persist-only).
	ApplyLogLevel func(level string) error

	// ApplyStorageNodes, when set, live-replaces the daemon's storage-node
	// list when a settings update changes storage_nodes (wired to the stacks
	// PinResolver). nil keeps the settings domain pure (persist-only).
	ApplyStorageNodes func(nodes string) error
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
	cluster.SettingSSOGitHubRepos(),
	cluster.SettingSSOCookieExpire(),
	cluster.SettingEdgeLoginDisabled(),
	cluster.SettingDomain(),
	cluster.SettingOOEmail(),
	cluster.SettingTraefikAdminUser(),
	cluster.SettingPlatformNode(),
	cluster.SettingOOLogsRetentionDays(),
	cluster.SettingOOMetricsRetentionDays(),
	cluster.SettingOOTracesRetentionDays(),
	cluster.SettingBackupS3Endpoint(),
	cluster.SettingBackupS3Bucket(),
	cluster.SettingBackupS3AccessKey(),
	cluster.SettingBackupS3SecretKey(),
	cluster.SettingBackupS3Region(),
	cluster.SettingLogLevel(),
	cluster.SettingReconcileInterval(),
	cluster.SettingStorageNodes(),
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
		switch k {
		case cluster.SettingLogLevel():
			if l.ApplyLogLevel != nil {
				if err := l.ApplyLogLevel(v); err != nil {
					return nil, fmt.Errorf("apply log level %q: %w", v, err)
				}
			}
		case cluster.SettingStorageNodes():
			if l.ApplyStorageNodes != nil {
				if err := l.ApplyStorageNodes(v); err != nil {
					return nil, fmt.Errorf("apply storage nodes %q: %w", v, err)
				}
			}
		}
	}
	return l.Get(ctx)
}
