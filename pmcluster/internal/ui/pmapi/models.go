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
