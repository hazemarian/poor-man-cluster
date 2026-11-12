package cluster

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/logging"
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
	// Default "true" in swarm deployments: Traefik admin-auth (or SSO) already
	// gates /web, so the console login + Users CRUD are hidden. Local standalone
	// runs leave it unset (or "false") to keep password login.
	settingEdgeLoginDisabled = "edge_login_disabled"

	// settingVolumeRoot is the single host directory every container volume is
	// forced under (subpaths /<app>/<name>). Defaults to /var/stack/data.
	settingVolumeRoot = "volume_root"

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

	// settingBackupEnabled is the master switch for the whole backup pipeline.
	// The node's timer fires regardless; the agent asks the control plane
	// whether this hour is due and does nothing when the answer is no. Absent
	// means "true" so a cluster that has been backing up since before this
	// setting existed keeps backing up.
	settingBackupEnabled = "backup_enabled"

	// Parseable stream retention (days), applied by the one-shot provision
	// service. Unbounded retention is how a metrics-heavy cluster silently
	// grows to hundreds of GB — the values render into
	// LOGS/METRICS/TRACES_RETENTION_DAYS on the provision one-shot, which
	// calls the Parseable retention API from inside the swarm network.
	settingObservabilityLogsRetentionDays    = "observability_logs_retention_days"
	settingObservabilityMetricsRetentionDays = "observability_metrics_retention_days"
	settingObservabilityTracesRetentionDays  = "observability_traces_retention_days"

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
	// settingBackupKeepLocal decides whether a node keeps the whole dense
	// window on its own disk ("dense", the default) or one artifact per stack
	// ("minimal"). The copies that matter are remote.
	settingBackupKeepLocal = "backup_keep_local"

	// Daemon log verbosity: "debug" | "info" | "warn" | "error". It reaches
	// two places: the daemon reads it at startup AND applies it live when the
	// console saves the settings (runtime re-level via logger.SetLevel), and
	// `cluster update` renders it into the platform stack as
	// PMCLUSTER_LOG_LEVEL so the edge container logs at the same level.
	settingLogLevel = "log_level"

	// settingAppLogLevel is the DEFAULT log level for DEPLOYED APPS, not for
	// the platform: every service of every app stack gets LOG_LEVEL from its
	// manifest's `log_level`, else from this, else "warn". It is deliberately
	// a separate key from settingLogLevel — one knob would make "quiet the
	// daemon" and "quiet my apps" the same action.
	settingAppLogLevel = "app_log_level"

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

	// settingGitHubRunnersEnabled is the master switch for the platform's
	// self-hosted GitHub Actions runners. Off by default: a runner container
	// needs /var/run/docker.sock, which is root-equivalent on the host, so it
	// is never something the platform turns on for an operator.
	settingGitHubRunnersEnabled = "github_runners_enabled"

	// settingGitHubRunnerOrg is the GitHub organisation whose runner group this
	// platform registers one runner into. Org-level (not per-repo) because one
	// registration serves every repository the group selects instead of one
	// registration per repository. It is also the scope the group's visibility
	// is checked against — see settingGitHubRunnerGroup.
	settingGitHubRunnerOrg = "github_runner_org"

	// settingGitHubRunnerGroup is the org runner group the runner joins
	// (`--runnergroup`). A group is the only scope knob GitHub offers an
	// org-level runner, and its visibility decides which repositories may
	// schedule onto it. The platform creates it when absent with visibility
	// "selected" and refuses "all": an all-visible runner executes workflow
	// code from ANY repository in the organisation, including one an attacker
	// can push to — on a container holding the host's docker socket.
	settingGitHubRunnerGroup = "github_runner_group"

	// settingGitHubRunnerNodes is the comma-separated list of worker hostnames
	// allowed to run a runner. A runner is a LOCAL container on the node that is
	// listed for it, not a swarm service, so the list is the capacity model:
	// N nodes = N runners. Managers (and the leader in particular) are refused —
	// a build on the control plane's 1-vCPU manager marks the node Down and takes
	// the cluster's API with it.
	settingGitHubRunnerNodes = "github_runner_nodes"

	// settingGitHubRunnerSocketProxy makes runners talk to the platform's own
	// filtered docker socket (internal/socketproxy) instead of the host's real
	// one. Default true. The raw-socket fallback exists for debugging a build
	// against a daemon the filter does not cover; it is unsafe for any workflow
	// the operator does not control.
	settingGitHubRunnerSocketProxy = "github_runner_socket_proxy"

	// settingGitHubRunnerSocketPath is where the filtered socket lives on every
	// node. A unix socket, root:root 0660, bind-mounted read-write into the
	// runner: no host port, no overlay exposure.
	settingGitHubRunnerSocketPath = "github_runner_socket_path"

	// settingGitHubRunnerMemory caps each runner container. 2560m is the
	// value proven on real hardware: at 1500m govulncheck OOM-killed the
	// runner mid-job, at 2560m it completed. A "m"/"g" suffix is a Docker
	// memory quantity (2560m ≈ 2.5 GiB).
	//
	// It is the CLUSTER-WIDE DEFAULT. A worker whose RAM cannot host that cap
	// says so with an override instead (settingGitHubRunnerNodeMemory): one
	// shared cap fits one node and is permission to OOM the other.
	settingGitHubRunnerMemory = "github_runner_memory"

	// settingGitHubRunnerNodeMemory overrides the memory cap for individual
	// runner nodes: `host=cap,host=cap` (e.g. nxt-sw-3-w=1200m,
	// nxt-sw-6-w=2560m). A node with no entry keeps
	// settingGitHubRunnerMemory, so the setting is additive: an operator only
	// writes the nodes that differ.
	//
	// Why it exists: measured on the live fleet, one 2560m cap was applied to
	// nxt-sw-3-w (1839 MB) and nxt-sw-6-w (3852 MB). The cap fits one and is
	// 139 % of the other, so the memory-fit rule refused 3-w — correctly, but
	// with no way to say "1200m there, 2560m here". A refusal that cannot be
	// fixed per node is an idle worker, not a boundary.
	settingGitHubRunnerNodeMemory = "github_runner_node_memory"

	// settingGitHubRunnerAllowLeader lifts the default placement constraint
	// (node.labels.pmcluster.leader != true). Runners build images and
	// compete with the control plane for CPU, so they stay off the leader
	// unless the operator says otherwise — a single-node cluster has to.
	settingGitHubRunnerAllowLeader = "github_runner_allow_leader"

	// settingGitHubRunnerAssignmentInterval is how often a node re-reads its
	// runner assignment from the leader, in seconds. It travels to the node
	// inside the assignment itself: a worker cannot read cluster settings
	// (configs are manager-only), so the leader sends the interval it is
	// configured with.
	settingGitHubRunnerAssignmentInterval = "github_runner_assignment_interval"

	// settingSwarmID records the live Swarm cluster ID (Raft cluster identity)
	// so `cluster update` can detect a wiped + re-initialised Swarm. Deliberately
	// NOT in the settings allowlist — it is written directly via SetSetting and
	// never surfaced for operator editing.
	settingSwarmID = "swarm_id"
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
func SettingBackupRetentionDays() string            { return settingBackupRetentionDays }
func SettingBackupCron() string                     { return settingBackupCron }
func SettingBackupEnabled() string                  { return settingBackupEnabled }
func SettingObservabilityLogsRetentionDays() string { return settingObservabilityLogsRetentionDays }
func SettingObservabilityMetricsRetentionDays() string {
	return settingObservabilityMetricsRetentionDays
}
func SettingObservabilityTracesRetentionDays() string { return settingObservabilityTracesRetentionDays }
func SettingBackupS3Endpoint() string                 { return settingBackupS3Endpoint }
func SettingBackupS3Bucket() string                   { return settingBackupS3Bucket }
func SettingBackupS3AccessKey() string                { return settingBackupS3AccessKey }
func SettingBackupS3SecretKey() string                { return settingBackupS3SecretKey }
func SettingBackupS3Region() string                   { return settingBackupS3Region }
func SettingBackupStoreOn() string                    { return settingBackupStoreOn }

// SettingBackupKeepLocal is the node-disk retention mode.
func SettingBackupKeepLocal() string   { return settingBackupKeepLocal }
func SettingLogLevel() string          { return settingLogLevel }
func SettingAppLogLevel() string       { return settingAppLogLevel }
func SettingReconcileInterval() string { return settingReconcileInterval }
func SettingStorageNodes() string      { return settingStorageNodes }
func SettingStorageFailover() string   { return settingStorageFailover }

// The self-hosted runner settings, exposed for the setup wizard and the
// runner reconcile loop (which read them through the store directly).
func SettingGitHubRunnersEnabled() string    { return settingGitHubRunnersEnabled }
func SettingGitHubRunnerOrg() string         { return settingGitHubRunnerOrg }
func SettingGitHubRunnerGroup() string       { return settingGitHubRunnerGroup }
func SettingGitHubRunnerNodes() string       { return settingGitHubRunnerNodes }
func SettingGitHubRunnerSocketProxy() string { return settingGitHubRunnerSocketProxy }

// settingGitHubRunnerDinD gives each job a docker daemon of its own, inside a
// privileged sibling container that is removed when the job ends, instead of the
// platform's filtered socket.
//
// It exists because the filtered socket only ever supported `docker build`: a job
// that runs `docker compose`, testcontainers or anything needing exec, networks or
// volumes was blocked route by route. DinD hands the job a real daemon and keeps
// the host out of it.
// The two keys below live in a block, not as standalone `const X = "…"` lines:
// internal/docscheck extracts documented keys with a regex anchored on a line that
// STARTS with `setting`, and a `const` prefix makes the key invisible — so the docs
// page and the code disagree while both look right. Keeping the shape uniform is what
// makes that check meaningful.
const (
	settingGitHubRunnerDinD = "github_runner_dind"

	// settingNodeDockerRestart lets the node's root watchdog restart docker.service
	// when the daemon stops answering.
	//
	// The pmcluster daemon can restart itself but never dockerd — dockerd is outside its
	// control loop — so this is the only mechanism that recovers a node whose docker died
	// under it. On 2026-11-12 nxt-sw-3-w sat Down for hours in exactly that state.
	//
	// Default false, deliberately: the remedy stops every container on the node,
	// including a running CI job and the platform services scheduled there. An operator
	// turns it on knowing that; an upgrade never turns it on for them.
	settingNodeDockerRestart = "node_docker_restart"
)

// SettingNodeDockerRestart is the setting key.
func SettingNodeDockerRestart() string { return settingNodeDockerRestart }

// LoadNodeDockerRestart reports whether the watchdog may restart docker.
func LoadNodeDockerRestart(ctx context.Context, st *store.Store) bool {
	if st == nil {
		return false
	}
	return strings.ToLower(strings.TrimSpace(st.SettingDefault(ctx, settingNodeDockerRestart, "false"))) == "true"
}

// SettingGitHubRunnerDinD is the setting key.
func SettingGitHubRunnerDinD() string { return settingGitHubRunnerDinD }

// LoadGitHubRunnerDinD reports whether runners get their own daemon.
// Default true: the socket proxy remains available for operators who want the
// narrow, filtered surface, but it is no longer the default a workflow meets.
func LoadGitHubRunnerDinD(ctx context.Context, st *store.Store) bool {
	if st == nil {
		return true
	}
	return strings.ToLower(strings.TrimSpace(st.SettingDefault(ctx, settingGitHubRunnerDinD, "true"))) != "false"
}
func SettingGitHubRunnerSocketPath() string  { return settingGitHubRunnerSocketPath }
func SettingGitHubRunnerMemory() string      { return settingGitHubRunnerMemory }
func SettingGitHubRunnerNodeMemory() string  { return settingGitHubRunnerNodeMemory }
func SettingGitHubRunnerAllowLeader() string { return settingGitHubRunnerAllowLeader }
func SettingGitHubRunnerAssignmentInterval() string {
	return settingGitHubRunnerAssignmentInterval
}

// DefaultGitHubRunnerSocketPath is where internal/socketproxy listens on every
// node. /run is tmpfs: the socket cannot outlive a reboot, which is the desired
// lifecycle for a credential-like path.
const DefaultGitHubRunnerSocketPath = "/run/pmcluster/docker.sock"

// LoadGitHubRunnerNodes returns the persisted node list verbatim (the caller
// parses it; keeping the raw string preserves the operator's order).
func LoadGitHubRunnerNodes(ctx context.Context, st *store.Store) string {
	if st == nil {
		return ""
	}
	return strings.TrimSpace(st.SettingDefault(ctx, settingGitHubRunnerNodes, ""))
}

// LoadGitHubRunnerSocketProxy reports whether runners use the filtered socket
// (github_runner_socket_proxy). Unset means ON: the raw socket is the debug
// fallback, not the default.
func LoadGitHubRunnerSocketProxy(ctx context.Context, st *store.Store) bool {
	if st == nil {
		return true
	}
	return strings.ToLower(strings.TrimSpace(st.SettingDefault(ctx, settingGitHubRunnerSocketProxy, "true"))) != "false"
}

// LoadGitHubRunnerSocketPath returns the filtered socket path, defaulting to
// DefaultGitHubRunnerSocketPath.
func LoadGitHubRunnerSocketPath(ctx context.Context, st *store.Store) string {
	if st == nil {
		return DefaultGitHubRunnerSocketPath
	}
	if v := strings.TrimSpace(st.SettingDefault(ctx, settingGitHubRunnerSocketPath, "")); v != "" {
		return v
	}
	return DefaultGitHubRunnerSocketPath
}

// DefaultGitHubRunnerMemory is the runner memory limit when
// github_runner_memory is unset. 2560m is what completed a govulncheck run on
// real hardware after 1500m was OOM-killed; lowering it is how a CI job dies
// halfway with a runner that looks healthy from the outside.
const DefaultGitHubRunnerMemory = "2560m"

// DefaultGitHubRunnerGroup is the runner group the platform creates when
// github_runner_group is unset. Named after the platform so an operator can see
// at a glance which group its runners live in.
const DefaultGitHubRunnerGroup = "pmcluster"

// LoadGitHubRunnerGroup returns the persisted runner group, falling back to
// DefaultGitHubRunnerGroup when unset or blank.
func LoadGitHubRunnerGroup(ctx context.Context, st *store.Store) string {
	if st == nil {
		return DefaultGitHubRunnerGroup
	}
	if v := strings.TrimSpace(st.SettingDefault(ctx, settingGitHubRunnerGroup, "")); v != "" {
		return v
	}
	return DefaultGitHubRunnerGroup
}

// LoadGitHubRunnerOrg returns the persisted organisation, trimmed, or "".
func LoadGitHubRunnerOrg(ctx context.Context, st *store.Store) string {
	if st == nil {
		return ""
	}
	return strings.TrimSpace(st.SettingDefault(ctx, settingGitHubRunnerOrg, ""))
}

// LoadGitHubRunnersEnabled reports whether the platform manages self-hosted
// runners (github_runners_enabled=true). Absent means off.
func LoadGitHubRunnersEnabled(ctx context.Context, st *store.Store) bool {
	if st == nil {
		return false
	}
	return st.SettingDefault(ctx, settingGitHubRunnersEnabled, "") == "true"
}

// LoadGitHubRunnerMemory returns the persisted runner memory limit, falling
// back to DefaultGitHubRunnerMemory when unset or blank. The value reaches
// Docker verbatim, so it is left as the operator wrote it.
func LoadGitHubRunnerMemory(ctx context.Context, st *store.Store) string {
	if st == nil {
		return DefaultGitHubRunnerMemory
	}
	if v := strings.TrimSpace(st.SettingDefault(ctx, settingGitHubRunnerMemory, "")); v != "" {
		return v
	}
	return DefaultGitHubRunnerMemory
}

// LoadGitHubRunnerNodeMemory returns the persisted per-node memory overrides
// verbatim (`host=cap,host=cap`). The caller parses it (runners.ParseNodeMemory)
// and resolves one node's cap with runners.Config.CapFor; an empty value means
// "every node uses github_runner_memory".
func LoadGitHubRunnerNodeMemory(ctx context.Context, st *store.Store) string {
	if st == nil {
		return ""
	}
	return strings.TrimSpace(st.SettingDefault(ctx, settingGitHubRunnerNodeMemory, ""))
}

// LoadGitHubRunnerAllowLeader reports whether runners may be placed on the
// leader (github_runner_allow_leader=true). Absent means no.
func LoadGitHubRunnerAllowLeader(ctx context.Context, st *store.Store) bool {
	if st == nil {
		return false
	}
	return st.SettingDefault(ctx, settingGitHubRunnerAllowLeader, "") == "true"
}

// DefaultGitHubRunnerAssignmentIntervalSeconds is how often a node re-reads its
// runner assignment when the setting is unset. One minute is fast enough that an
// operator's change lands while they are still looking at the console, and slow
// enough that a fleet of workers costs the leader one request a minute each.
const DefaultGitHubRunnerAssignmentIntervalSeconds = 60

// LoadGitHubRunnerAssignmentInterval returns the poll interval in seconds,
// clamped to [10, 3600]: below ten would hammer the leader's GitHub quota, and an
// hour is the point at which a stale assignment is worse than no poll at all. A
// value that does not parse falls back to the default rather than to zero — a
// typo must not turn the poll off.
func LoadGitHubRunnerAssignmentInterval(ctx context.Context, st *store.Store) int {
	raw := ""
	if st != nil {
		raw = strings.TrimSpace(st.SettingDefault(ctx, settingGitHubRunnerAssignmentInterval, ""))
	}
	if raw == "" {
		return DefaultGitHubRunnerAssignmentIntervalSeconds
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return DefaultGitHubRunnerAssignmentIntervalSeconds
	}
	if n < 10 {
		return 10
	}
	if n > 3600 {
		return 3600
	}
	return n
}

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
// is disabled (default true — the swarm deployment always gates /web behind
// Traefik admin-auth or SSO, so the console login is redundant; local standalone
// runs set edge_login_disabled=false to keep password login).
func loadEdgeLoginDisabled(ctx context.Context, st *store.Store) bool {
	if st == nil {
		return true
	}
	return st.SettingDefault(ctx, settingEdgeLoginDisabled, "true") == "true"
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

// Parseable per-stream retention defaults (days). They mirror the provision
// script's own fallback (embeds/observability-provision.sh) so a cluster whose
// operator never touched a setting keeps today's behaviour, and a missing env
// var at the one-shot also keeps it. The two must never drift apart.
const (
	defaultLogsRetentionDays    = 30
	defaultMetricsRetentionDays = 14
	defaultTracesRetentionDays  = 10
)

// DefaultObservabilityLogsRetentionDays reports the built-in log retention
// default so the settings surface can show the value actually in effect.
func DefaultObservabilityLogsRetentionDays() int { return defaultLogsRetentionDays }

// DefaultObservabilityMetricsRetentionDays reports the built-in metric
// retention default.
func DefaultObservabilityMetricsRetentionDays() int { return defaultMetricsRetentionDays }

// DefaultObservabilityTracesRetentionDays reports the built-in trace retention
// default.
func DefaultObservabilityTracesRetentionDays() int { return defaultTracesRetentionDays }

// loadObservabilityRetention returns the Parseable log/metric/trace retention
// in days. Each falls back to its per-stream default when unset or unparseable.
func loadObservabilityRetention(ctx context.Context, st *store.Store) (logs, metrics, traces int) {
	if st == nil {
		return defaultLogsRetentionDays, defaultMetricsRetentionDays, defaultTracesRetentionDays
	}
	parse := func(key string, def int) int {
		v := st.SettingDefault(ctx, key, "")
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return def
		}
		return n
	}
	return parse(settingObservabilityLogsRetentionDays, defaultLogsRetentionDays),
		parse(settingObservabilityMetricsRetentionDays, defaultMetricsRetentionDays),
		parse(settingObservabilityTracesRetentionDays, defaultTracesRetentionDays)
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
//
// Three, matching backups.DefaultRetention and the value the setup wizard
// offers, because a fallback that disagrees with the documented default is a
// silent policy: a cluster whose operator never opened the setting kept a
// fortnight of artifacts while every surface said three days. The dense 48
// hours inside those three days is not affected — this number only governs the
// sparse tail.
const defaultBackupRetentionDays = 3

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

// LoadLogLevel returns the platform's own log level (the log_level setting)
// as a name the logger accepts, defaulting to info. A stored value this build
// does not recognise degrades to the default instead of rendering a
// PMCLUSTER_LOG_LEVEL the daemon and the edge would both have to guess at.
func LoadLogLevel(ctx context.Context, st *store.Store) string {
	return loadLevelSetting(ctx, st, settingLogLevel, logging.DefaultLevel)
}

// LoadAppLogLevel returns the DEFAULT log level for deployed apps (the
// app_log_level setting), defaulting to warn. Manifests that set their own
// `log_level` — or a service that declares LOG_LEVEL — outrank it.
func LoadAppLogLevel(ctx context.Context, st *store.Store) string {
	return loadLevelSetting(ctx, st, settingAppLogLevel, logging.DefaultAppLevel)
}

// loadLevelSetting reads one level-valued setting and normalizes it.
func loadLevelSetting(ctx context.Context, st *store.Store, key, def string) string {
	if st == nil {
		return def
	}
	return logging.LevelOrDefault(st.SettingDefault(ctx, key, ""), def)
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
