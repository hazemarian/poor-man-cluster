package pmapi

// This file mirrors the JSON shapes returned by the pmcluster daemon
// (internal/api). The UI decodes into these so controllers render typed data
// rather than raw JSON.

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
}

// Stack is one row of GET /api/stacks.
type Stack struct {
	Name            string `json:"name"`
	CurrentRevision int64  `json:"current_revision"`
	RepoURL         string `json:"repo_url"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

// RevisionMeta is a lightweight revision entry (no YAML bodies).
type RevisionMeta struct {
	Revision  int64 `json:"revision"`
	CreatedAt int64 `json:"created_at"`
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

// DeployPayload is POST /api/stacks body.
type DeployPayload struct {
	AppName  string `json:"app_name,omitempty"`
	RepoURL  string `json:"repo_url,omitempty"`
	Version  string `json:"version,omitempty"`
	Manifest string `json:"manifest"`
}

// DeployResult is the POST /api/stacks response.
type DeployResult struct {
	Stack    string `json:"stack"`
	Revision int64  `json:"revision"`
}

// RollbackResult is the POST /api/stacks/{name}/rollback response.
type RollbackResult struct {
	Stack        string `json:"stack"`
	NewRevision  int64  `json:"new_revision"`
	RolledBackTo int64  `json:"rolled_back_to"`
}

// HostCert is one per-host TLS certificate managed via /api/tls/hosts. Times
// are RFC3339 strings (Go json marshals time.Time as such).
type HostCert struct {
	Host      string   `json:"host"`
	SANs      []string `json:"sans"`
	NotAfter  string   `json:"not_after"`
	NotBefore string   `json:"not_before"`
	CertFile  string   `json:"cert_file"`
	KeyFile   string   `json:"key_file"`
}

// hostCertsResponse is GET /api/tls/hosts.
type hostCertsResponse struct {
	Hosts []HostCert `json:"hosts"`
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

// APIKey is one API user row (no token material — the token is only returned
// once at creation via APIKeyCreated).
type APIKey struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
}

// APIKeyCreated is the POST /api/api_keys response — it carries the one-time
// bearer token.
type APIKeyCreated struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

// Secret is one row of GET /api/secrets. Payload is never returned — only the
// content hash is exposed for verification.
type Secret struct {
	ID        int64  `json:"id"`
	Scope     string `json:"scope"`
	Name      string `json:"name"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"created_at"`
}

// Config is one row of GET /api/configs.
type Config struct {
	ID        int64  `json:"id"`
	Scope     string `json:"scope"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Version   string `json:"version"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	Content   string `json:"content,omitempty"`
}

// ConfigVersion is one history row of GET /api/configs/{name}/versions.
type ConfigVersion struct {
	ID        int64  `json:"id"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"created_at"`
}
