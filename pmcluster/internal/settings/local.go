package settings

import (
	"context"
	"fmt"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/logging"
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
	cluster.SettingBackupRetentionDays(),
	cluster.SettingBackupCron(),
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
	cluster.SettingObservabilityLogsRetentionDays(),
	cluster.SettingObservabilityMetricsRetentionDays(),
	cluster.SettingObservabilityTracesRetentionDays(),
	cluster.SettingBackupS3Endpoint(),
	cluster.SettingBackupS3Bucket(),
	cluster.SettingBackupS3AccessKey(),
	cluster.SettingBackupS3SecretKey(),
	cluster.SettingBackupS3Region(),
	cluster.SettingBackupStoreOn(),
	cluster.SettingBackupKeepLocal(),
	cluster.SettingBackupEnabled(),
	cluster.SettingLogLevel(),
	cluster.SettingAppLogLevel(),
	cluster.SettingReconcileInterval(),
	cluster.SettingStorageNodes(),
	cluster.SettingStorageFailover(),
}

// settingDefaults is what Get reports for a key the platform has a built-in
// default for. Reporting the default (rather than "") means the console and
// the CLI show the value actually in effect, so "unset" can never be mistaken
// for "0"/"off"/"the loudest possible logging".
var settingDefaults = map[string]string{
	cluster.SettingLogLevel():    logging.DefaultLevel,
	cluster.SettingAppLogLevel(): logging.DefaultAppLevel,
}

// Get returns the current value of every known setting ("", or its default
// when the platform has one).
func (l *Local) Get(ctx context.Context) (Settings, error) {
	settings := make(Settings, len(clusterSettingKeys))
	for _, k := range clusterSettingKeys {
		settings[k] = l.Store.SettingDefault(ctx, k, settingDefaults[k])
	}
	return settings, nil
}

// stackPinPrefix is the key prefix for a per-stack placement pin
// (stacks.StackPinKey). Kept as a literal so this package need not depend on
// the stacks package.
const stackPinPrefix = "stack_pin_"

// Update persists the given settings and returns the updated snapshot.
// Unknown keys and invalid values are rejected atomically (nothing is written
// when any key is unknown or any value is refused).
func (l *Local) Update(ctx context.Context, values Settings) (Settings, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("settings: required")
	}
	allowed := map[string]bool{}
	for _, k := range clusterSettingKeys {
		allowed[k] = true
	}
	normalized := make(Settings, len(values))
	for k, v := range values {
		// A per-stack placement pin is written by the platform itself (first
		// placement, and `stack move`), so it is a legitimate setting an
		// operator must be able to inspect and clear. It cannot be enumerated
		// in advance because it is per stack.
		if !allowed[k] && !strings.HasPrefix(k, stackPinPrefix) {
			return nil, fmt.Errorf("unknown setting: %s", k)
		}
		if isLevelKey(k) {
			// `warning` is accepted as an alias of warn everywhere else, so
			// accepting it here and storing the canonical name keeps one
			// spelling in the DB and in the rendered env.
			n := logging.Normalize(v)
			if v != "" && n == "" {
				return nil, fmt.Errorf("%s: must be one of %s, got %q", k, logging.AllowedNames, v)
			}
			normalized[k] = n
			continue
		}
		normalized[k] = v
	}
	for k, v := range normalized {
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

// isLevelKey reports whether a setting holds a log level. Both keys are
// level-valued and validated identically; they differ only in whose logging
// they control (`log_level` the platform, `app_log_level` the deployed apps).
func isLevelKey(key string) bool {
	return key == cluster.SettingLogLevel() || key == cluster.SettingAppLogLevel()
}
