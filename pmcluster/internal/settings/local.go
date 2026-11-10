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
	cluster.SettingGitHubRunnerOrg(),
	cluster.SettingGitHubRunnerGroup(),
	cluster.SettingGitHubRunnerNodes(),
	cluster.SettingGitHubRunnerSocketProxy(),
	cluster.SettingGitHubRunnerSocketPath(),
	cluster.SettingGitHubRunnerMemory(),
	cluster.SettingGitHubRunnerAllowLeader(),
	cluster.SettingGitHubRunnerAssignmentInterval(),
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
	// A feature that fronts the host's docker socket is never on by default.
	cluster.SettingGitHubRunnersEnabled():           "false",
	cluster.SettingGitHubRunnerGroup():              cluster.DefaultGitHubRunnerGroup,
	cluster.SettingGitHubRunnerSocketProxy():        "true",
	cluster.SettingGitHubRunnerSocketPath():         cluster.DefaultGitHubRunnerSocketPath,
	cluster.SettingGitHubRunnerMemory():             cluster.DefaultGitHubRunnerMemory,
	cluster.SettingGitHubRunnerAllowLeader():        "false",
	cluster.SettingGitHubRunnerAssignmentInterval(): strconv.Itoa(cluster.DefaultGitHubRunnerAssignmentIntervalSeconds),
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
	// Cross-field check: an ENABLED feature needs an App, an org and at least one
	// node, however the update was split across calls.
	if err := l.validateRunnerEnablement(ctx, normalized); err != nil {
		return nil, err
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
		cluster.SettingGitHubRunnerOrg(),
		cluster.SettingGitHubRunnerGroup(),
		cluster.SettingGitHubRunnerNodes(),
		cluster.SettingGitHubRunnerSocketProxy(),
		cluster.SettingGitHubRunnerSocketPath(),
		cluster.SettingGitHubRunnerMemory(),
		cluster.SettingGitHubRunnerAllowLeader(),
		cluster.SettingGitHubRunnerAssignmentInterval():
		return true
	}
	return false
}

// normalizeRunnerSetting validates and canonicalises one runner setting.
//
// The enabled flag is gated on the App registration HERE, in the store-backed
// adapter every surface goes through (CLI, REST, console), so no caller can turn
// the feature on without the credential it depends on. The org/node requirements
// are checked once the whole update is normalised (validateRunnerEnablement),
// because they may arrive in the same request or already be stored.
//
// The dependency is real, not stylistic: a runner registers with an org-scoped,
// one-hour App installation token. A personal access token could do the same job
// only by holding administration:write on the whole organisation — a long-lived
// credential far wider than the token this platform mints — so it is refused by
// name rather than accepted as a quieter substitute.
func (l *Local) normalizeRunnerSetting(ctx context.Context, key, value string) (string, error) {
	switch key {
	case cluster.SettingGitHubRunnersEnabled():
		on, err := parseBoolSetting(value)
		if err != nil {
			return "", fmt.Errorf("%s: %w", key, err)
		}
		if on && !l.githubAppConfigured(ctx) {
			return "", fmt.Errorf("%s requires a configured GitHub App: self-hosted runners register with an App installation token (`pmcluster github app set --app-id … --key-file …`). A personal access token is not an acceptable substitute — it would need administration:write on the whole organisation, a long-lived credential with a far wider blast radius than the hour-long installation token the App mints", key)
		}
		return strconv.FormatBool(on), nil
	case cluster.SettingGitHubRunnerOrg():
		if strings.TrimSpace(value) == "" {
			// Clearing the org is allowed only while the feature is off; the
			// pairing check refuses the combination with enabled=true.
			return "", nil
		}
		org, err := runners.NormalizeOrg(value)
		if err != nil {
			return "", fmt.Errorf("%s: %w", key, err)
		}
		return org, nil
	case cluster.SettingGitHubRunnerGroup():
		group, err := runners.NormalizeGroup(value)
		if err != nil {
			return "", fmt.Errorf("%s: %w", key, err)
		}
		return group, nil
	case cluster.SettingGitHubRunnerNodes():
		if strings.TrimSpace(value) == "" {
			return "", nil
		}
		nodes, err := runners.NormalizeNodes(value)
		if err != nil {
			return "", fmt.Errorf("%s: %w", key, err)
		}
		return nodes, nil
	case cluster.SettingGitHubRunnerSocketProxy():
		on, err := parseBoolSetting(value)
		if err != nil {
			return "", fmt.Errorf("%s: %w", key, err)
		}
		return strconv.FormatBool(on), nil
	case cluster.SettingGitHubRunnerSocketPath():
		path := strings.TrimSpace(value)
		if path == "" {
			return "", nil
		}
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, " 	\r\n") {
			return "", fmt.Errorf("%s must be an absolute path, got %q", key, value)
		}
		return path, nil
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
	case cluster.SettingGitHubRunnerAssignmentInterval():
		raw := strings.TrimSpace(value)
		if raw == "" {
			return "", nil
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return "", fmt.Errorf("%s: must be a whole number of seconds, got %q", key, value)
		}
		// The floor is not cosmetic: a per-node poll is one request to this
		// leader, and each enabled answer mints a GitHub registration token and
		// verifies the runner group's visibility. Ten seconds is the fastest an
		// operator may make a fleet ask.
		if n < 10 || n > 3600 {
			return "", fmt.Errorf("%s: must be between 10 and 3600 seconds, got %d", key, n)
		}
		return strconv.Itoa(n), nil
	}
	return value, nil
}

// validateRunnerEnablement refuses an ENABLED feature that cannot work: no App,
// no organisation, or no selected node. It runs on the normalised update with the
// stored values filling any key the request did not send, so "enable" and "clear
// the org" are both caught whichever order they arrive in.
func (l *Local) validateRunnerEnablement(ctx context.Context, normalized Settings) error {
	effective := func(key string) string {
		if v, ok := normalized[key]; ok {
			return v
		}
		if l.Store == nil {
			return ""
		}
		return l.Store.SettingDefault(ctx, key, settingDefaults[key])
	}
	if !strings.EqualFold(strings.TrimSpace(effective(cluster.SettingGitHubRunnersEnabled())), "true") {
		return nil
	}
	if !l.githubAppConfigured(ctx) {
		return fmt.Errorf("%s requires a configured GitHub App: runners register with an installation token (`pmcluster github app set --app-id … --key-file …`)", cluster.SettingGitHubRunnersEnabled())
	}
	if strings.TrimSpace(effective(cluster.SettingGitHubRunnerOrg())) == "" {
		return fmt.Errorf("%s is enabled but %s is empty: an org-level runner registers into an organisation, so set github_runner_org (for example acme) or turn the feature off", cluster.SettingGitHubRunnersEnabled(), cluster.SettingGitHubRunnerOrg())
	}
	if strings.TrimSpace(effective(cluster.SettingGitHubRunnerNodes())) == "" {
		return fmt.Errorf("%s is enabled but %s is empty: a runner is a local container on a worker, so select at least one worker node (the Runners panel has a node picker) or turn the feature off", cluster.SettingGitHubRunnersEnabled(), cluster.SettingGitHubRunnerNodes())
	}
	return nil
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
