package settings

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/logging"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runners"
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
	cluster.SettingGitHubRunnersEnabled(),
	cluster.SettingGitHubRunnerRepos(),
	cluster.SettingGitHubRunnerMemory(),
	cluster.SettingGitHubRunnerAllowLeader(),
}

// settingDefaults is what Get reports for a key the platform has a built-in
// default for. Reporting the default (rather than "") means the console and
// the CLI show the value actually in effect, so "unset" can never be mistaken
// for "0"/"off"/"the loudest possible logging".
var settingDefaults = map[string]string{
	cluster.SettingLogLevel():                          logging.DefaultLevel,
	cluster.SettingAppLogLevel():                       logging.DefaultAppLevel,
	cluster.SettingObservabilityLogsRetentionDays():    strconv.Itoa(cluster.DefaultObservabilityLogsRetentionDays()),
	cluster.SettingObservabilityMetricsRetentionDays(): strconv.Itoa(cluster.DefaultObservabilityMetricsRetentionDays()),
	cluster.SettingObservabilityTracesRetentionDays():  strconv.Itoa(cluster.DefaultObservabilityTracesRetentionDays()),
	// A feature that mounts /var/run/docker.sock is never on by default.
	cluster.SettingGitHubRunnersEnabled():    "false",
	cluster.SettingGitHubRunnerMemory():      cluster.DefaultGitHubRunnerMemory,
	cluster.SettingGitHubRunnerAllowLeader(): "false",
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
		if isRetentionKey(k) {
			// Retention is a whole number of days, one or more. A zero or
			// negative value would silently disable the retention bound that
			// keeps a metrics-heavy cluster from growing without limit.
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 1 {
				return nil, fmt.Errorf("%s: must be a whole number of days, 1 or more, got %q", k, v)
			}
			normalized[k] = strconv.Itoa(n)
			continue
		}
		if isGitHubRunnerKey(k) {
			n, err := l.normalizeRunnerSetting(ctx, k, v)
			if err != nil {
				return nil, err
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

// isRetentionKey reports whether a setting holds a Parseable stream retention
// in days. The three keys are validated identically as whole numbers >= 1.
func isRetentionKey(key string) bool {
	return key == cluster.SettingObservabilityLogsRetentionDays() ||
		key == cluster.SettingObservabilityMetricsRetentionDays() ||
		key == cluster.SettingObservabilityTracesRetentionDays()
}

// isGitHubRunnerKey reports whether a setting belongs to the self-hosted
// runner surface.
func isGitHubRunnerKey(key string) bool {
	switch key {
	case cluster.SettingGitHubRunnersEnabled(),
		cluster.SettingGitHubRunnerRepos(),
		cluster.SettingGitHubRunnerMemory(),
		cluster.SettingGitHubRunnerAllowLeader():
		return true
	}
	return false
}

// normalizeRunnerSetting validates and canonicalises one runner setting.
//
// The enabled flag is gated on the App registration HERE, in the store-backed
// adapter every surface goes through (CLI, REST, console), so no caller can
// turn the feature on without the credential it depends on. The dependency is
// real, not stylistic: a runner registers with a repo-scoped, one-hour App
// installation token. A personal access token could do the same job only by
// holding administration:write on every repository — a long-lived credential
// far wider than the token this platform mints — so it is refused by name
// rather than accepted as a quieter substitute.
func (l *Local) normalizeRunnerSetting(ctx context.Context, key, value string) (string, error) {
	switch key {
	case cluster.SettingGitHubRunnersEnabled():
		on, err := parseBoolSetting(value)
		if err != nil {
			return "", fmt.Errorf("%s: %w", key, err)
		}
		if on && !l.githubAppConfigured(ctx) {
			return "", fmt.Errorf("%s requires a configured GitHub App: self-hosted runners register with an App installation token (`pmcluster github app set --app-id … --key-file …`). A personal access token is not an acceptable substitute — it would need administration:write on every repository, a long-lived credential with a far wider blast radius than the hour-long installation token the App mints", key)
		}
		return strconv.FormatBool(on), nil
	case cluster.SettingGitHubRunnerRepos():
		return runners.NormalizeRepos(value)
	case cluster.SettingGitHubRunnerMemory():
		// Empty means "use the platform default" (2560m), matching every other
		// setting whose empty value is a built-in default.
		if strings.TrimSpace(value) == "" {
			return "", nil
		}
		if err := runners.ValidateMemory(value); err != nil {
			return "", fmt.Errorf("%s: %w", key, err)
		}
		return strings.TrimSpace(value), nil
	case cluster.SettingGitHubRunnerAllowLeader():
		on, err := parseBoolSetting(value)
		if err != nil {
			return "", fmt.Errorf("%s: %w", key, err)
		}
		return strconv.FormatBool(on), nil
	}
	return value, nil
}

// githubAppConfigured reports whether an App registration exists. A nil store
// is "not configured": without the store there is no credential to read.
func (l *Local) githubAppConfigured(ctx context.Context) bool {
	if l.Store == nil {
		return false
	}
	_, err := l.Store.GitHubApp(ctx)
	return err == nil
}

// parseBoolSetting accepts the spellings a bool setting arrives in: an empty
// value (a cleared checkbox), the words, and 1/0 from a script.
func parseBoolSetting(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "false", "0", "no", "off":
		return false, nil
	case "true", "1", "yes", "on":
		return true, nil
	}
	return false, fmt.Errorf("must be true or false, got %q", v)
}
