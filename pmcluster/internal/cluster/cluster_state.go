package cluster

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// cluster_settings keys persisted on a successful `up` and read back by both
// `up` (idempotent re-runs) and `update` (knows what to re-provision without
// re-bootstrapping).
const (
	settingTLSMode            = "tls_mode"
	settingTLSCertPath        = "tls_cert_path"
	settingTLSKeyPath         = "tls_key_path"
	settingTLSACME            = "tls_acme_email"
	settingDomain             = "domain"
	settingObservabilityEmail = "observability_admin_email"
	settingTraefikAdminUser   = "traefik_admin_user"

	// SSO settings. SSO is a feature flag (settingSSOEnabled): when enabled the
	// Traefik admin-auth basicAuth middleware is swapped for a forwardAuth
	// middleware pointing at the oauth2-proxy sidecar (sso stack), so identity
	// comes from the OAuth provider (GitHub) instead of the htpasswd file.
	settingSSOEnabled      = "sso_enabled"
	settingSSOProvider     = "sso_provider"
	settingSSOClientID     = "sso_client_id"
	settingSSOClientSecret = "sso_client_secret"
	settingSSOGitHubOrg    = "sso_github_org"
	settingSSOGitHubRepos  = "sso_github_repos"
	settingSSOCookieExpire = "sso_cookie_expire"

	// settingEdgeLoginDisabled controls the edge console's own session login.
	// An unset value keeps the legacy Traefik Basic Auth gate ("true"), so an
	// existing cluster is not switched under its operators on upgrade. Setup
	// writes this setting explicitly and defaults new clusters to native
	// sessions; "false" selects the native cookie session.
	settingEdgeLoginDisabled = "edge_login_disabled"

	// settingVolumeRoot is the single host directory every container volume is
	// forced under (subpaths /<app>/<name>). Defaults to /var/stack/data.
	settingVolumeRoot = "volume_root"

	// settingBackupAllNodes makes the backup agent run on every swarm node
	// (manager-only by default). When replicated disks are in use it must stay
	// false — see docs/storage-and-databases.md.
	settingBackupAllNodes = "backup_all_nodes"

	// settingBackupRetentionDays is how long pmcluster keeps backup audit rows
	// and their archives on disk before pruning them (default 15 days). The
	// offen agent's own BACKUP_RETENTION_DAYS is rendered from this same value.
	settingBackupRetentionDays = "backup_retention_days"

	// settingBackupCron is the cron expression the offen backup agent uses for
	// its scheduled runs (rendered into BACKUP_CRON_EXPRESSION). Standard
	// 5-field cron. Defaults to hourly ("0 * * * *") so an offsite archive is
	// never more than an hour old — the storage-failover path restores the
	// newest of these. Set e.g. "0 3 * * *" for the old daily 03:00 cadence.
	settingBackupCron = "backup_cron"

	// settingPlatformNode pins every platform service (traefik, openobserve,
	// edge, backup, sso) to ONE specific node hostname. Empty keeps the role
	// constraint node.role == manager. Set it to match the stateful-app
	// placement rule so platform state under /var/stack/data can never be
	// rescheduled to a node with empty storage.
	settingPlatformNode = "platform_node"

	// OpenObserve stream retention (days). Unbounded retention is how a
	// metrics-heavy cluster silently grows to hundreds of GB — the values
	// render into ZO_LOGS/METRICS/TRACES_RETENTION_DAYS.
	settingObservabilityLogsRetentionDays    = "observability_logs_retention_days"
	settingObservabilityMetricsRetentionDays = "observability_metrics_retention_days"
	settingObservabilityTracesRetentionDays  = "observability_traces_retention_days"

	// Host ports for the platform ingress. Defaults are the standard 80/443;
	// alternate ports let pmcluster run beside an existing ingress controller
	// on hosts that already own the standard ports.
	settingIngressHTTPPort  = "ingress_http_port"
	settingIngressHTTPSPort = "ingress_https_port"

	// Offsite (S3/R2) backup destination for the volume-backup agent. When all
	// five are set, offen also uploads every archive to the S3-compatible
	// endpoint (Cloudflare R2, MinIO, AWS...). Stored like the other secret
	// settings (sso_client_secret) — plaintext in the store, masked in the
	// console.
	settingBackupS3Endpoint  = "backup_s3_endpoint"
	settingBackupS3Bucket    = "backup_s3_bucket"
	settingBackupS3AccessKey = "backup_s3_access_key"
	settingBackupS3SecretKey = "backup_s3_secret_key"
	settingBackupS3Region    = "backup_s3_region"

	// settingBackupStoreOn pins the in-cluster backup store (SeaweedFS) to a
	// specific node class: "leader" (the manager/leader node) or "worker" (a
	// non-storage worker, the default). Empty means "worker"/auto for
	// back-compat. The store is normally kept off storage nodes; an explicit
	// "leader" choice is allowed as-is.
	settingBackupStoreOn = "backup_store_on"

	// Daemon log verbosity: "debug" | "info" | "warn" | "error". Rendered into
	// nothing — the daemon reads it at startup AND applies it live when the
	// console saves the cluster settings (runtime re-level via logger.SetLevel).
	settingLogLevel = "log_level"

	// settingReconcileInterval is how often the leader daemon runs the
	// control-plane reconcile loop (0 disables the loop entirely).
	settingReconcileInterval = "reconcile_interval"

	// settingStorageNodes is a comma-separated list of node hostnames that
	// stateful app stacks (services with volumes and no explicit placement)
	// are round-robined across. Explicit placement always wins; when a stack
	// is individually pinned (stack move) that pin outranks the round-robin;
	// platform_node remains the single-node fallback when storage_nodes is
	// unset.
	settingStorageNodes = "storage_nodes"

	// settingStorageFailover controls automatic storage failover. When "true"
	// the control loop moves a stateful stack off a failed storage node onto a
	// healthy storage node (restoring the latest offsite backup). When false
	// (the default) it only alerts — the stack stays paused until an operator
	// acts. Automatic failover requires offsite backups (backup_s3_*); without
	// them it is refused. Enable via the setup prompt or
	// `cluster settings set storage_failover=true`.
	settingStorageFailover = "storage_failover"

	// settingSwarmID records the live Swarm cluster ID (Raft cluster identity)
	// so `cluster update` can detect a wiped + re-initialised Swarm. Deliberately
	// NOT in the settings allowlist — it is written directly via SetSetting and
	// never surfaced for operator editing.
	settingSwarmID = "swarm_id"

	// settingObservabilityParseableHostname pins the Parseable task hostname.
	// Parseable's local-store catalog includes the task hostname, so leaving it
	// to Docker's per-task ID can make a Swarm replacement look like a new
	// backend. The value is internal state, captured from an existing service
	// once and rendered into every later observability reconcile.
	settingObservabilityParseableHostname = "observability_parseable_hostname"
)

// Setting* accessors expose the persisted settings keys for CLI surfaces
// (e.g. the interactive `pmcluster setup` wizard) that read/write them
// directly instead of through the cluster workflow.
func SettingDomain() string                         { return settingDomain }
func SettingTLSMode() string                        { return settingTLSMode }
func SettingTLSCertPath() string                    { return settingTLSCertPath }
func SettingTLSKeyPath() string                     { return settingTLSKeyPath }
func SettingTLSACME() string                        { return settingTLSACME }
func SettingOOEmail() string                        { return settingObservabilityEmail }
func SettingTraefikAdminUser() string               { return settingTraefikAdminUser }
func SettingSSOEnabled() string                     { return settingSSOEnabled }
func SettingSSOProvider() string                    { return settingSSOProvider }
func SettingSSOClientID() string                    { return settingSSOClientID }
func SettingSSOClientSecret() string                { return settingSSOClientSecret }
func SettingSSOGitHubOrg() string                   { return settingSSOGitHubOrg }
func SettingSSOGitHubRepos() string                 { return settingSSOGitHubRepos }
func SettingSSOCookieExpire() string                { return settingSSOCookieExpire }
func SettingEdgeLoginDisabled() string              { return settingEdgeLoginDisabled }
func SettingVolumeRoot() string                     { return settingVolumeRoot }
func SettingBackupAllNodes() string                 { return settingBackupAllNodes }
func SettingBackupRetentionDays() string            { return settingBackupRetentionDays }
func SettingBackupCron() string                     { return settingBackupCron }
func SettingPlatformNode() string                   { return settingPlatformNode }
func SettingObservabilityLogsRetentionDays() string { return settingObservabilityLogsRetentionDays }
func SettingObservabilityMetricsRetentionDays() string {
	return settingObservabilityMetricsRetentionDays
}
func SettingObservabilityTracesRetentionDays() string { return settingObservabilityTracesRetentionDays }
func SettingIngressHTTPPort() string                  { return settingIngressHTTPPort }
func SettingIngressHTTPSPort() string                 { return settingIngressHTTPSPort }
func SettingBackupS3Endpoint() string                 { return settingBackupS3Endpoint }
func SettingBackupS3Bucket() string                   { return settingBackupS3Bucket }
func SettingBackupS3AccessKey() string                { return settingBackupS3AccessKey }
func SettingBackupS3SecretKey() string                { return settingBackupS3SecretKey }
func SettingBackupS3Region() string                   { return settingBackupS3Region }
func SettingBackupStoreOn() string                    { return settingBackupStoreOn }
func SettingLogLevel() string                         { return settingLogLevel }
func SettingReconcileInterval() string                { return settingReconcileInterval }
func SettingStorageNodes() string                     { return settingStorageNodes }
func SettingStorageFailover() string                  { return settingStorageFailover }

// SettingObservabilityParseableHostname returns the internal setting key used
// to keep the native Parseable task hostname stable across Swarm replacements.
func SettingObservabilityParseableHostname() string { return settingObservabilityParseableHostname }

// ClusterInstalled reports whether this store already holds a live cluster.
func ClusterInstalled(ctx context.Context, st *store.Store) bool {
	ok, err := clusterInstalled(ctx, st)
	return err == nil && ok
}

// clusterInstalled reports whether this store already holds a live cluster.
// `cluster up` is init-only, so it refuses to run when either the persisted
// domain/TLS install state or the cluster-scope platform config rows exist.
func clusterInstalled(ctx context.Context, st *store.Store) (bool, error) {
	if st == nil {
		return false, nil
	}
	if st.SettingDefault(ctx, settingDomain, "") != "" {
		return true, nil
	}
	if st.SettingDefault(ctx, settingTLSMode, "") != "" {
		return true, nil
	}
	rows, err := st.ListConfigs(ctx, "cluster", "")
	if err != nil {
		return false, fmt.Errorf("list cluster configs: %w", err)
	}
	return len(rows) > 0, nil
}

// tlsState is the persisted TLS install state.
type tlsState struct {
	Mode      string
	CertPath  string
	KeyPath   string
	ACMEEmail string
}

// loadTLSSettings reads the persisted TLS state (empty when never installed).
func loadTLSSettings(ctx context.Context, st *store.Store) (tlsState, error) {
	if st == nil {
		return tlsState{}, nil
	}
	return tlsState{
		Mode:      st.SettingDefault(ctx, settingTLSMode, ""),
		CertPath:  st.SettingDefault(ctx, settingTLSCertPath, ""),
		KeyPath:   st.SettingDefault(ctx, settingTLSKeyPath, ""),
		ACMEEmail: st.SettingDefault(ctx, settingTLSACME, ""),
	}, nil
}

func (t tlsState) save(ctx context.Context, st *store.Store) error {
	if st == nil {
		return nil
	}
	vals := map[string]string{
		settingTLSMode:     t.Mode,
		settingTLSCertPath: t.CertPath,
		settingTLSKeyPath:  t.KeyPath,
		settingTLSACME:     t.ACMEEmail,
	}
	for k, v := range vals {
		if err := st.SetSetting(ctx, k, v); err != nil {
			return fmt.Errorf("persist %s: %w", k, err)
		}
	}
	return nil
}

// TLSState is the exported view of the persisted TLS install state, used by
// CLI surfaces (the `pmcluster setup` wizard) to write the TLS mode before
// handing off to cluster up/update.
type TLSState struct {
	Mode      string
	CertPath  string
	KeyPath   string
	ACMEEmail string
}

// Save persists the TLS install state into the store settings.
func (t TLSState) Save(ctx context.Context, st *store.Store) error {
	return tlsState(t).save(ctx, st)
}

// ssoState is the persisted SSO install state. SSO is opt-in via the
// sso_enabled flag; the provider is always "github" for now (the only
// provider oauth2-proxy is configured with).
type ssoState struct {
	Enabled      bool
	Provider     string
	ClientID     string
	ClientSecret string
	GitHubOrg    string
	// GitHubRepos optionally restricts sign-in to users with access to the
	// given comma-separated repositories (e.g.
	// "nextrum-sy/donation-campaign,nextrum-sy/donation-campaign-frontend").
	// Rendered as oauth2-proxy's OAUTH2_PROXY_GITHUB_REPOS (plural) flag.
	GitHubRepos string
	// CookieExpire is the oauth2-proxy session cookie lifetime (e.g. "1h",
	// "24h", "168h"). Default "1h" so a member removed from the GitHub org
	// loses access within the hour instead of keeping a 7-day session.
	CookieExpire string
}

// defaultSSOCookieExpire is the tightened default session lifetime used when
// sso_cookie_expire is unset (oauth2-proxy's own default is 168h).
const defaultSSOCookieExpire = "1h"

// loadSSOSettings reads the persisted SSO state (all empty when never set).
func loadSSOSettings(ctx context.Context, st *store.Store) (ssoState, error) {
	if st == nil {
		return ssoState{}, nil
	}
	return ssoState{
		Enabled:      st.SettingDefault(ctx, settingSSOEnabled, "") == "true",
		Provider:     st.SettingDefault(ctx, settingSSOProvider, ""),
		ClientID:     st.SettingDefault(ctx, settingSSOClientID, ""),
		ClientSecret: st.SettingDefault(ctx, settingSSOClientSecret, ""),
		GitHubOrg:    st.SettingDefault(ctx, settingSSOGitHubOrg, ""),
		GitHubRepos:  st.SettingDefault(ctx, settingSSOGitHubRepos, ""),
		CookieExpire: st.SettingDefault(ctx, settingSSOCookieExpire, defaultSSOCookieExpire),
	}, nil
}

func (s ssoState) validate() error {
	if !s.Enabled {
		return nil
	}
	if s.Provider == "" {
		return fmt.Errorf("SSO is enabled but no provider is configured (only \"github\" is supported)")
	}
	if s.Provider != "github" {
		return fmt.Errorf("SSO provider %q is not supported (only \"github\" is supported)", s.Provider)
	}
	if s.ClientID == "" || s.ClientSecret == "" {
		return fmt.Errorf("SSO provider %q requires client ID + client secret", s.Provider)
	}
	if _, err := time.ParseDuration(s.CookieExpire); err != nil {
		return fmt.Errorf("SSO cookie expire %q is not a valid duration (e.g. 1h, 24h, 168h): %w", s.CookieExpire, err)
	}
	return nil
}

// loadEdgeLoginDisabled reports whether the edge console's own session login
// is disabled, i.e. whether the edge gates /web/* with Traefik Basic Auth.
// A missing setting, or a missing store, keeps Basic Auth: that is the legacy
// behaviour, so a cluster that predates the setting does not change its login
// on upgrade. Setup always writes edge_login_disabled explicitly, so fresh
// clusters get the native session chosen at setup time. edge_login_disabled=false
// selects the native cookie session.
func loadEdgeLoginDisabled(ctx context.Context, st *store.Store) bool {
	if st == nil {
		return true
	}
	// SSO owns the browser gate. Keep the edge's own session disabled in that
	// mode regardless of edge_login_disabled; otherwise operators would face two
	// independent login systems and the SSO sign-out path would be misleading.
	if st.SettingDefault(ctx, settingSSOEnabled, "") == "true" {
		return true
	}
	return st.SettingDefault(ctx, settingEdgeLoginDisabled, "true") == "true"
}

// loadBackupAllNodes reports whether the volume-backup agent should run on
// every swarm node (false = manager-only, the default and the required mode
// when disks are synchronously replicated — see storage guide).
func loadBackupAllNodes(ctx context.Context, st *store.Store) bool {
	if st == nil {
		return false
	}
	return st.SettingDefault(ctx, settingBackupAllNodes, "") == "true"
}

// loadPlatformNode returns the hostname platform services are pinned to
// (empty = keep the node.role == manager constraint).
func loadPlatformNode(ctx context.Context, st *store.Store) string {
	if st == nil {
		return ""
	}
	return st.SettingDefault(ctx, settingPlatformNode, "")
}

const defaultObservabilityParseableHostname = "pmcluster-parseable"

// loadObservabilityParseableHostname returns the persisted Parseable hostname,
// capturing an explicitly configured hostname from an existing service when a
// legacy cluster has not recorded it yet. Fresh installs receive a stable
// name instead of Docker's per-task container ID. The setting is deliberately
// internal and is not part of the editable settings surface. A missing service
// may default only during an explicitly fresh `cluster up`; installed update
// paths fail closed so a surviving data volume cannot be rebound with a new
// hostname.
func loadObservabilityParseableHostname(ctx context.Context, st *store.Store, d runtime.Client, allowFreshDefault bool) (string, error) {
	if st == nil {
		if !allowFreshDefault {
			return "", fmt.Errorf("read %s: state store is required for an installed cluster", settingObservabilityParseableHostname)
		}
		return defaultObservabilityParseableHostname, nil
	}
	current, settingErr := st.Setting(ctx, settingObservabilityParseableHostname)
	if settingErr == nil {
		if current = strings.TrimSpace(current); current != "" {
			return current, nil
		}
	} else if !errors.Is(settingErr, store.ErrSettingNotFound) {
		return "", fmt.Errorf("read %s: %w", settingObservabilityParseableHostname, settingErr)
	}

	if d == nil {
		if !allowFreshDefault {
			return "", fmt.Errorf("inspect observability_parseable: runtime client is required for an installed cluster")
		}
		return persistObservabilityParseableHostname(ctx, st, defaultObservabilityParseableHostname)
	}

	svc, inspectErr := d.ServiceInspect(ctx, "observability_parseable")
	if inspectErr != nil {
		if errors.Is(inspectErr, runtime.ErrServiceNotFound) {
			if !allowFreshDefault {
				return "", fmt.Errorf("observability_parseable service is missing on an installed cluster; refusing to default its hostname")
			}
			return persistObservabilityParseableHostname(ctx, st, defaultObservabilityParseableHostname)
		}
		return "", fmt.Errorf("inspect observability_parseable: %w", inspectErr)
	}
	if hostname := strings.TrimSpace(svc.Hostname); hostname != "" {
		return persistObservabilityParseableHostname(ctx, st, hostname)
	}
	reader, ok := d.(runtime.RunningServiceHostnameReader)
	if !ok {
		return "", fmt.Errorf("observability_parseable has no task-template hostname and runtime cannot inspect its running container hostname")
	}
	hostname, err := reader.ServiceRunningHostname(ctx, "observability_parseable")
	if err != nil {
		return "", fmt.Errorf("cannot determine observability_parseable's hostname to preserve its catalog identity (%w); make sure exactly one observability_parseable task is running (docker service ps observability_parseable) and retry", err)
	}
	if strings.TrimSpace(hostname) == "" {
		return "", fmt.Errorf("running observability_parseable container reported an empty hostname")
	}
	return persistObservabilityParseableHostname(ctx, st, strings.TrimSpace(hostname))
}

func persistObservabilityParseableHostname(ctx context.Context, st *store.Store, hostname string) (string, error) {
	if err := st.SetSetting(ctx, settingObservabilityParseableHostname, hostname); err != nil {
		return "", fmt.Errorf("persist %s: %w", settingObservabilityParseableHostname, err)
	}
	return hostname, nil
}

// LoadStorageFailover reports whether automatic storage failover is enabled
// (storage_failover=true). Exported so the reconcile loop can gate its
// automatic move on the same setting the setup wizard writes.
func LoadStorageFailover(ctx context.Context, st *store.Store) bool {
	if st == nil {
		return false
	}
	return st.SettingDefault(ctx, settingStorageFailover, "") == "true"
}

// defaultOORetentionDays is the OpenObserve stream retention fallback when
// the setting is unset. Kept short: metrics are the disk hog (a histogram
// stream can grow into tens of GB in weeks) — unbounded retention is how the
// legacy cluster accumulated a 123G /data volume.
const defaultOORetentionDays = 7

// LoadIngressPorts returns the host ports published by the platform ingress.
// Values outside 1-65535 fall back to 80/443; the two ports must differ.
func LoadIngressPorts(ctx context.Context, st *store.Store) (httpPort, httpsPort int) {
	httpPort, httpsPort = 80, 443
	if st == nil {
		return httpPort, httpsPort
	}
	parse := func(key string, fallback int) int {
		v := st.SettingDefault(ctx, key, "")
		if v == "" {
			return fallback
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return fallback
		}
		return n
	}
	httpPort = parse(settingIngressHTTPPort, 80)
	httpsPort = parse(settingIngressHTTPSPort, 443)
	if httpPort == httpsPort {
		return 80, 443
	}
	return httpPort, httpsPort
}

// loadObservabilityRetention returns the OpenObserve log/metric/trace retention in days.
// Each falls back to defaultOORetentionDays when unset or unparseable.
func loadObservabilityRetention(ctx context.Context, st *store.Store) (logs, metrics, traces int) {
	if st == nil {
		return defaultOORetentionDays, defaultOORetentionDays, defaultOORetentionDays
	}
	parse := func(key string) int {
		v := st.SettingDefault(ctx, key, "")
		if v == "" {
			return defaultOORetentionDays
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return defaultOORetentionDays
		}
		return n
	}
	return parse(settingObservabilityLogsRetentionDays),
		parse(settingObservabilityMetricsRetentionDays),
		parse(settingObservabilityTracesRetentionDays)
}

// BackupS3 is the offsite backup destination for the volume-backup agent.
// When Endpoint, Bucket, AccessKey and SecretKey are all non-empty, the offen
// agent also uploads every archive to the S3-compatible endpoint (Cloudflare
// R2, MinIO, AWS S3...). Region defaults to "auto" (R2) when empty.
type BackupS3 struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
}

// Configured reports whether a full offsite destination has been set.
func (b BackupS3) Configured() bool {
	return b.Endpoint != "" && b.Bucket != "" && b.AccessKey != "" && b.SecretKey != ""
}

// EndpointHost returns the endpoint with any URL scheme stripped, so offen's
// AWS_ENDPOINT gets a bare host:port while AWS_ENDPOINT_PROTO carries the
// scheme (a scheme in AWS_ENDPOINT is rejected by offen).
func (b BackupS3) EndpointHost() string {
	s := strings.TrimPrefix(b.Endpoint, "https://")
	return strings.TrimPrefix(s, "http://")
}

// EndpointProto returns the scheme to pass as offen's AWS_ENDPOINT_PROTO:
// "https" for an https endpoint, "http" otherwise.
func (b BackupS3) EndpointProto() string {
	if strings.HasPrefix(b.Endpoint, "https://") {
		return "https"
	}
	return "http"
}

// loadBackupS3 returns the persisted offsite backup destination.
func loadBackupS3(ctx context.Context, st *store.Store) BackupS3 {
	if st == nil {
		return BackupS3{}
	}
	return BackupS3{
		Endpoint:  st.SettingDefault(ctx, settingBackupS3Endpoint, ""),
		Bucket:    st.SettingDefault(ctx, settingBackupS3Bucket, ""),
		AccessKey: st.SettingDefault(ctx, settingBackupS3AccessKey, ""),
		SecretKey: st.SettingDefault(ctx, settingBackupS3SecretKey, ""),
		Region:    st.SettingDefault(ctx, settingBackupS3Region, "auto"),
	}
}

// loadBackupStoreOn returns where the in-cluster backup store should run:
// "leader" or "worker". Empty means "worker"/auto (the pre-setting behavior)
// so older clusters keep working unchanged.
func loadBackupStoreOn(ctx context.Context, st *store.Store) string {
	if st == nil {
		return ""
	}
	return st.SettingDefault(ctx, settingBackupStoreOn, "")
}

// defaultBackupRetentionDays is how many days backup audit rows + archives
// are kept before pmcluster prunes them.
const defaultBackupRetentionDays = 15

// LoadBackupRetentionDays returns the persisted backup retention window in
// days, falling back to defaultBackupRetentionDays when unset or unparseable.
func LoadBackupRetentionDays(ctx context.Context, st *store.Store) int {
	if st == nil {
		return defaultBackupRetentionDays
	}
	v := st.SettingDefault(ctx, settingBackupRetentionDays, "")
	if v == "" {
		return defaultBackupRetentionDays
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return defaultBackupRetentionDays
	}
	return n
}

// defaultBackupCron is the backup agent's default schedule: hourly, on the
// hour. Hourly keeps the offsite archive at most one hour stale, which is what
// the storage-failover path restores.
const defaultBackupCron = "0 * * * *"

// LoadBackupCron returns the persisted backup cron expression, falling back to
// defaultBackupCron (hourly) when unset or blank. The value is a standard
// 5-field cron expression passed straight to the offen agent.
func LoadBackupCron(ctx context.Context, st *store.Store) string {
	if st == nil {
		return defaultBackupCron
	}
	v := strings.TrimSpace(st.SettingDefault(ctx, settingBackupCron, ""))
	if v == "" {
		return defaultBackupCron
	}
	return v
}

// requestedTLSMode derives the TLS mode the operator asked for on this run,
func requestedTLSMode(in UpInput) string {
	if in.ACMEEmail != "" {
		return "acme"
	}
	if in.CertPath != "" || in.KeyPath != "" {
		return "cert"
	}
	return ""
}

// CertResolverForMode returns the ACME resolver name to attach to DSL-exposed
// Traefik routers for the given TLS mode ("acme" → "letsencrypt", anything
// else → ""). BYO-cert clusters never define a letsencrypt resolver, so
// emitting the label there would break routing.
func CertResolverForMode(tlsMode string) string {
	if tlsMode == "acme" {
		return "letsencrypt"
	}
	return ""
}

// mergeTLSState reconciles the operator's requested input with the stored
// install state so an idempotent re-run doesn't require re-passing the TLS
// flags and can't silently flip the TLS mode.
//
//   - No stored state → input is used as-is.
//   - Stored state + no TLS flags on this run → fill in the stored cert/key
//     paths (cert mode) or ACME email (acme mode).
//   - Stored mode != requested mode → error unless ForceTLSMode is set.
//   - Stored mode == requested mode → any flags supplied win, others are
//     filled from storage.
func mergeTLSState(in UpInput, state tlsState) (UpInput, error) {
	if state.Mode == "" {
		return in, nil
	}
	req := requestedTLSMode(in)
	if req != "" && req != state.Mode && !in.ForceTLSMode {
		return in, fmt.Errorf("cluster is installed with TLS mode %q but you requested %q; pass --force-tls-mode to switch (an accidental mode flip can permanently break the edge)", state.Mode, req)
	}
	switch state.Mode {
	case "cert":
		if in.CertPath == "" {
			in.CertPath = state.CertPath
		}
		if in.KeyPath == "" {
			in.KeyPath = state.KeyPath
		}
	case "acme":
		if in.ACMEEmail == "" {
			in.ACMEEmail = state.ACMEEmail
		}
	}
	return in, nil
}
