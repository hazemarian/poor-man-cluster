package pmapi

import "time"

// Me is GET /api/me — the authenticated daemon user.
type Me struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// ClusterInfo is GET /api/cluster/info.
type ClusterInfo struct {
	NodeName      string    `json:"node_name"`
	ServerVersion string    `json:"server_version"`
	OS            string    `json:"os"`
	Arch          string    `json:"arch"`
	CPUs          int       `json:"cpus"`
	MemoryBytes   int64     `json:"memory_bytes"`
	Swarm         SwarmInfo `json:"swarm"`
}

// SwarmInfo is the nested swarm summary in ClusterInfo.
type SwarmInfo struct {
	State            string `json:"state"`
	ControlAvailable bool   `json:"control_available"`
	Managers         int    `json:"managers"`
	Nodes            int    `json:"nodes"`
}

// Node is one row of GET /api/nodes.
type Node struct {
	ID            string `json:"id"`
	Hostname      string `json:"hostname"`
	Role          string `json:"role"`
	Availability  string `json:"availability"`
	Status        string `json:"status"`
	IsLeader      bool   `json:"is_leader"`
	EngineVersion string `json:"engine_version"`
	Address       string `json:"address"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
	Storage       bool   `json:"storage"`
}

// StackError is one deploy/apply outcome for a stack (newest first in a
// stack's LastError history; an empty Error means that apply succeeded).
type StackError struct {
	Revision  int64  `json:"revision"`
	Error     string `json:"error"`
	CreatedAt int64  `json:"created_at"`
}

// StackFailover is an open storage-failover marker on a stack: it was moved
// from FromNode to ToNode after a storage-node failure and stays unacknowledged
// until an operator acknowledges it or moves the stack back.
type StackFailover struct {
	StackName string `json:"StackName"`
	FromNode  string `json:"FromNode"`
	ToNode    string `json:"ToNode"`
	At        int64  `json:"At"`
	Acked     bool   `json:"Acked"`
}

// Stack is one row of GET /api/stacks.
type Stack struct {
	Name            string         `json:"name"`
	CurrentRevision int64          `json:"current_revision"`
	RepoURL         string         `json:"repo_url"`
	SourceFile      string         `json:"source_file"`
	LastError       []StackError   `json:"last_error"`
	Failover        *StackFailover `json:"failover"`
	CreatedAt       int64          `json:"created_at"`
	UpdatedAt       int64          `json:"updated_at"`
}

// RevisionMeta is a lightweight revision entry (no YAML bodies).
type RevisionMeta struct {
	Revision  int64 `json:"revision"`
	CreatedAt int64 `json:"created_at"`
	// Error is this deployment execution's outcome ("" = applied cleanly).
	Error string `json:"error"`
}

// StackDetail is GET /api/stacks/{name}.
type StackDetail struct {
	Stack      Stack          `json:"stack"`
	Revisions  []RevisionMeta `json:"revisions"`
	LastBackup *BackupSummary `json:"last_backup"`
}

// BackupSummary is the lightweight last-backup object inside StackDetail.
type BackupSummary struct {
	Status       string `json:"status"`
	StartedAt    int64  `json:"started_at"`
	FinishedAt   int64  `json:"finished_at,omitempty"`
	Revision     int64  `json:"revision,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// Revision is GET /api/stacks/{name}/revisions/{rev}.
type Revision struct {
	Stack        string `json:"stack"`
	Revision     int64  `json:"revision"`
	CreatedAt    int64  `json:"created_at"`
	SourceYAML   string `json:"source_yaml"`
	RenderedYAML string `json:"rendered_yaml"`
	Payload      string `json:"payload"`
	Error        string `json:"error"`
}

// Backup is one row of GET /api/backups (and per-stack backups).
type Backup struct {
	ID           int64    `json:"id"`
	Status       string   `json:"status"`
	StackName    string   `json:"stack_name"`
	Revision     int64    `json:"revision"`
	ArchivePaths []string `json:"archive_paths"`
	ErrorMessage string   `json:"error_message"`
	StartedAt    int64    `json:"started_at"`
	FinishedAt   int64    `json:"finished_at"`
}

// BackupFile is one entry inside a backup archive listing.
type BackupFile struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
}

// DeployPayload is POST /api/stacks body.
type DeployPayload struct {
	AppName  string `json:"app_name,omitempty"`
	RepoURL  string `json:"repo_url,omitempty"`
	File     string `json:"file,omitempty"`
	Version  string `json:"version,omitempty"`
	Manifest string `json:"manifest"`
}

// DeployResult is the POST /api/stacks response.
type DeployResult struct {
	Stack    string `json:"stack"`
	Revision int64  `json:"revision"`
	Changed  bool   `json:"changed"`
}

// RollbackResult is the POST /api/stacks/{name}/rollback response.
type RollbackResult struct {
	Stack        string `json:"stack"`
	NewRevision  int64  `json:"new_revision"`
	RolledBackTo int64  `json:"rolled_back_to"`
}

// HostCert is one per-host TLS certificate managed via /api/tls/hosts. Per-host
// certs live in versioned Swarm secrets + a site_certs DB row (the same table
// and flow as the cluster's own certificate); times are RFC3339 strings.
type HostCert struct {
	Host       string   `json:"host"`
	SANs       []string `json:"sans"`
	NotAfter   string   `json:"not_after"`
	NotBefore  string   `json:"not_before"`
	CertSecret string   `json:"cert_secret"`
	KeySecret  string   `json:"key_secret"`
	CertHash   string   `json:"cert_hash"`
	KeyHash    string   `json:"key_hash"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
}

// hostCertsResponse is GET /api/tls/hosts.
type hostCertsResponse struct {
	Hosts []HostCert `json:"hosts"`
}

// SiteCert is GET/PUT /api/tls/site — metadata for the cluster's own
// (default/wildcard) certificate. Times are RFC3339 strings.
type SiteCert struct {
	Domain     string   `json:"domain"`
	CertSecret string   `json:"cert_secret"`
	KeySecret  string   `json:"key_secret"`
	NotBefore  string   `json:"not_before"`
	NotAfter   string   `json:"not_after"`
	SANs       []string `json:"sans"`
	CertHash   string   `json:"cert_hash"`
	KeyHash    string   `json:"key_hash"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
}

// Webhook is one deploy-webhook source row (no secret — the HMAC secret is
// only returned once at creation via WebhookCreated).
type Webhook struct {
	Source      string `json:"source"`
	Description string `json:"description,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	LastUsedAt  int64  `json:"last_used_at,omitempty"`
}

// WebhookCreated is the POST /api/webhooks response — it carries the
// one-time-only shared secret.
type WebhookCreated struct {
	Source string `json:"source"`
	Secret string `json:"secret"`
}

// WebhookDelivery is one recorded delivery attempt for a webhook source.
type WebhookDelivery struct {
	ID        int64  `json:"id"`
	Source    string `json:"source"`
	Status    string `json:"status"`
	StackName string `json:"stack_name,omitempty"`
	Revision  int64  `json:"revision,omitempty"`
	RepoURL   string `json:"repo_url,omitempty"`
	File      string `json:"file,omitempty"`
	Error     string `json:"error,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// APIKey is one API user row (no token material — the token is only returned
// once at creation via APIKeyCreated). Stack is the optional per-token scope:
// non-empty means the token may only drive that one stack.
type APIKey struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at"`
	Stack      string `json:"stack"`
}

// APIKeyCreated is the POST /api/api_keys response — it carries the one-time
// bearer token.
type APIKeyCreated struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Stack string `json:"stack"`
	Token string `json:"token"`
}

// GitHubAppStatus is GET /api/github/app. The private key and installation
// tokens never appear here — only ids, a fingerprint of the key, the last probe
// outcome and the granted permission map.
type GitHubAppStatus struct {
	Configured      bool              `json:"configured"`
	AppID           int64             `json:"app_id"`
	InstallationID  int64             `json:"installation_id"`
	ProbeRepo       string            `json:"probe_repo"`
	ProbePackage    string            `json:"probe_package"`
	Slug            string            `json:"slug"`
	Permissions     map[string]string `json:"permissions"`
	Warning         string            `json:"warning"`
	KeyFingerprint  string            `json:"key_fingerprint"`
	LastProbeOK     bool              `json:"last_probe_ok"`
	LastProbeDetail string            `json:"last_probe_detail"`
	LastProbeAt     int64             `json:"last_probe_at"`
}

// GitHubAppTest is POST /api/github/app/test. A probe that fails is a normal
// answer with OK=false and the cause in Message.
type GitHubAppTest struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// GitHubAppSet is POST /api/github/app. Zero ids and an empty key mean "keep
// what is stored"; a nil probe field keeps the stored target, an empty string
// clears it.
type GitHubAppSet struct {
	AppID          int64   `json:"app_id,omitempty"`
	InstallationID int64   `json:"installation_id,omitempty"`
	PrivateKeyPEM  string  `json:"private_key_pem,omitempty"`
	ProbeRepo      *string `json:"probe_repo,omitempty"`
	ProbePackage   *string `json:"probe_package,omitempty"`
}

// GitHubAppSetResult is what the daemon answers with: the ids, the fingerprint
// of the key it stored (never the key) and what the probe proved.
type GitHubAppSetResult struct {
	AppID          int64          `json:"app_id"`
	InstallationID int64          `json:"installation_id"`
	KeyFingerprint string         `json:"key_fingerprint"`
	Probe          GitHubAppProbe `json:"probe"`
}

// GitHubAppProbe is the probe half of a set result. OK=false with a detail that
// says "no probe target configured" means the token minted but no read ran.
type GitHubAppProbe struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Secret is one row of GET /api/secrets. Payload is never returned — only the
// content hash is exposed for verification.
type Secret struct {
	ID        int64  `json:"id"`
	Scope     string `json:"scope"`
	Stack     string `json:"stack,omitempty"`
	Name      string `json:"name"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"created_at"`
}

// SecretValue is GET /api/secrets/{name}/value — the decrypted plaintext,
// returned only to the authenticated operator on explicit request.
type SecretValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Config is one row of GET /api/configs.
type Config struct {
	ID        int64  `json:"id"`
	Scope     string `json:"scope"`
	Stack     string `json:"stack,omitempty"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Version   string `json:"version"`
	Hash      string `json:"hash"`
	Rendered  bool   `json:"rendered,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	Content   string `json:"content,omitempty"`
}

// UpdateSummary is POST /api/update — the result of re-running `cluster
// update` from the Settings page (applies edited cluster configs/secrets to
// the Swarm side).
type UpdateSummary struct {
	OTelConfig     string   `json:"otel_config"`
	TraefikConfig  string   `json:"traefik_config"`
	CertSecret     string   `json:"cert_secret"`
	KeySecret      string   `json:"key_secret"`
	EdgeConfig     string   `json:"edge_config"`
	StacksDeployed []string `json:"stacks_deployed"`
	// ConfigsChanged lists the rendered platform configs (OTel collector,
	// Traefik dynamic, observability-provision, edge fingerprint) replaced on
	// this update. Additive key: a console that ignores it keeps working, and
	// one that reads it can tell a config swap from a true no-op.
	ConfigsChanged []string `json:"configs_changed"`
}

// RenderedConfig is one of the cluster's platform configs after template
// substitution — the YAML actually sent to the Swarm (stacks, collector config,
// Traefik dynamic config). Read-only; rendered on demand from the daemon.
type RenderedConfig struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type renderedConfigsResponse struct {
	Configs []RenderedConfig `json:"configs"`
}

// NodeStatProc is one process in a node's top list (GET /api/node-stats).
type NodeStatProc struct {
	PID    int     `json:"pid"`
	Name   string  `json:"name"`
	RSSMB  float64 `json:"rss_mb"`
	CPUPct float64 `json:"cpu_pct"`
}

// NodeStat is one node's newest sample (GET /api/node-stats). One per node:
// the daemon upserts the row, so this list never grows a history.
type NodeStat struct {
	Hostname   string         `json:"hostname"`
	Role       string         `json:"role"`
	Cores      int            `json:"cores"`
	Load1      float64        `json:"load1"`
	Load5      float64        `json:"load5"`
	Load15     float64        `json:"load15"`
	MemTotalMB int64          `json:"mem_total_mb"`
	MemUsedMB  int64          `json:"mem_used_mb"`
	MemAvailMB int64          `json:"mem_avail_mb"`
	DockerUp   bool           `json:"docker_up"`
	TopRSS     []NodeStatProc `json:"top_rss"`
	TopCPU     []NodeStatProc `json:"top_cpu"`
	SampleTS   int64          `json:"sample_ts"`
	UpdatedAt  int64          `json:"updated_at"`
}

// ClusterSettings is GET/PUT /api/cluster/settings — the editable cluster
// settings keyed by name (e.g. "volume_root", "sso_enabled", "domain").
type ClusterSettings map[string]string

// Usage is GET /api/usage — which stacks reference which configs and secrets,
// computed from the latest rendered compose per stack.
type Usage struct {
	Configs map[string][]string `json:"configs"`
	Secrets map[string][]string `json:"secrets"`
}

// ConfigVersion is one history row of GET /api/configs/{name}/versions.
type ConfigVersion struct {
	ID        int64  `json:"id"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"created_at"`
}

// Service is one row of GET /api/services (or GET /api/services/{stack}).
type Service struct {
	Name         string `json:"name"`
	Stack        string `json:"stack"`
	Replicas     uint64 `json:"replicas"`
	Desired      uint64 `json:"desired"`
	Image        string `json:"image"`
	Mode         string `json:"mode"`
	RunOnce      bool   `json:"run_once"`
	ImageCreated int64  `json:"image_created"`
	UpdateState  string `json:"update_state"`
	UpdateError  string `json:"update_error"`
	Updated      int64  `json:"updated"`
	Node         string `json:"node"`
	// RunningNode is the node the service's running task is on ("" when none
	// is running). Node is where it was placed; RunningNode is where it is.
	RunningNode string `json:"running_node"`
	Platform    bool   `json:"platform"`
}

// ServiceTask is one row of GET /api/services/{stack}/{svc}/tasks.
type ServiceTask struct {
	TaskID     string `json:"task_id"`
	Node       string `json:"node"`
	Slot       int64  `json:"slot"`
	State      string `json:"state"`
	Error      string `json:"error"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at"`
}

// ServiceLogLine is one row of GET /api/services/{stack}/{svc}/logs.
type ServiceLogLine struct {
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

// ExecResult is POST /api/services/{stack}/{svc}/exec.
type ExecResult struct {
	Service  string `json:"service"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// ApplyRecord is the last platform apply as the daemon reports it.
type ApplyRecord struct {
	Present      bool     `json:"present"`
	At           string   `json:"at"`
	Version      string   `json:"version"`
	Source       string   `json:"source"`
	Stacks       []string `json:"stacks"`
	Configs      []string `json:"configs"`
	Warnings     []string `json:"warnings"`
	WarningCount int      `json:"warning_count"`
	Line         string   `json:"line"`
}

// FleetVersions is the manager fleet's reported binary versions.
type FleetVersions struct {
	Reference string         `json:"reference"`
	Nodes     []VersionEntry `json:"nodes"`
	Stale     []VersionEntry `json:"stale"`
	OK        bool           `json:"ok"`
}

// VersionEntry is one manager's reported version.
type VersionEntry struct {
	Hostname string `json:"hostname"`
	Version  string `json:"version"`
}

// GitHubRunner is one row of GET /api/github/runners: a runner the platform
// manages, as the daemon observes it. State is derived (GitHub's runner list plus
// the local container), never guessed by the console.
type GitHubRunner struct {
	Name         string `json:"name"`
	Org          string `json:"org"`
	Node         string `json:"node"`
	ContainerID  string `json:"container_id"`
	Mode         string `json:"mode"`
	State        string `json:"state"`
	RegisteredAt int64  `json:"registered_at"`
	LastSeen     int64  `json:"last_seen"`
	LastJobURL   string `json:"last_job_url"`
	// Reason explains a row that is offline for a reason other than GitHub
	// saying so — today: the memory cap does not fit the node.
	Reason string `json:"reason"`
	// Caches are this runner's persistent cache volumes with their sizes, measured by
	// the daemon on its node. Empty for a runner elsewhere: unobservable, not zero.
	Caches []RunnerCache `json:"caches,omitempty"`
	// CacheTotal is those sizes added up, already formatted.
	CacheTotal string `json:"cache_total,omitempty"`
	// CacheWarnings are the caches past their ceiling, in words for the operator.
	CacheWarnings []string `json:"cache_warnings,omitempty"`
}

// RunnerCache is one persistent cache volume behind a runner.
type RunnerCache struct {
	Name   string `json:"name"`
	Volume string `json:"volume"`
	Bytes  int64  `json:"bytes"`
	Limit  int64  `json:"limit"`
}

// GitHubRunners is the list response. Enabled travels with the rows so the panel's
// empty state and the table cannot disagree.
type GitHubRunners struct {
	Enabled bool           `json:"enabled"`
	Runners []GitHubRunner `json:"runners"`
}

// GitHubRunnerKill is the outcome of a kill or restart. A GitHub failure is not an
// error status: the container is already gone and deregister_error names the half
// that did not happen.
type GitHubRunnerKill struct {
	Name              string `json:"name"`
	Org               string `json:"org"`
	Node              string `json:"node"`
	ContainerRemoved  bool   `json:"container_removed"`
	Deregistered      bool   `json:"deregistered"`
	DeregisterError   string `json:"deregister_error,omitempty"`
	Held              bool   `json:"held"`
	Registered        bool   `json:"registered,omitempty"`
	RegistrationError string `json:"registration_error,omitempty"`
}

// CIRun is one repository's latest workflow run, as GET /api/ci/runs reports it.
// It mirrors githubapp.WorkflowRun on purpose: the console model is a projection of
// the wire shape, so a new field is a deliberate addition rather than an accident.
type CIRun struct {
	ID           int64     `json:"id"`
	Repo         string    `json:"repo"`
	Workflow     string    `json:"workflow"`
	Event        string    `json:"event"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	HeadSHA      string    `json:"head_sha"`
	Branch       string    `json:"branch"`
	URL          string    `json:"html_url"`
	UpdatedAt    time.Time `json:"updated_at"`
	FailedJob    string    `json:"failed_job,omitempty"`
	FailedStep   string    `json:"failed_step,omitempty"`
	SupersededBy int64     `json:"superseded_by,omitempty"`
	Unreadable   string    `json:"unreadable,omitempty"`
}

// CISnapshot is GET /api/ci/runs: the rows plus the counts the header shows.
// FetchedAt is when the DATA was read, not when it was served — a cached snapshot
// says how stale it is instead of pretending to be live.
type CISnapshot struct {
	Runs      []CIRun   `json:"runs"`
	Queued    int       `json:"queued"`
	Running   int       `json:"running"`
	Failed    int       `json:"failed"`
	FetchedAt time.Time `json:"fetched_at"`
	Cached    bool      `json:"cached"`
	Error     string    `json:"error,omitempty"`
}
