// Package runtime is the neutral port contract pmcluster needs from a
// container orchestrator. Domain packages depend on THIS package, never on a
// concrete implementation (internal/docker is one adapter; a future k3s /
// Kubernetes backend would implement the same interface). The models here are
// deliberately small and runtime-agnostic — no Docker SDK types leak through.
package runtime

import "context"

// Client is the contract pmcluster needs from an orchestrator daemon — a
// subset of what Docker Swarm offers. *Exists/*Create methods collapse
// "not found" to (false, nil); idempotency is the caller's job. *Remove
// methods are idempotent (nil on missing).
type Client interface {
	Ping(ctx context.Context) (Ping, error)
	Info(ctx context.Context) (Info, error)

	NetworkExists(ctx context.Context, name string) (bool, error)
	NetworkCreate(ctx context.Context, spec NetworkSpec) error

	SecretExists(ctx context.Context, name string) (bool, error)
	SecretCreate(ctx context.Context, spec SecretSpec) error

	ConfigExists(ctx context.Context, name string) (bool, error)
	ConfigCreate(ctx context.Context, spec ConfigSpec) error

	SecretRemove(ctx context.Context, name string) error
	ConfigRemove(ctx context.Context, name string) error
	NetworkRemove(ctx context.Context, name string) error
	VolumeRemove(ctx context.Context, name string) error

	ServiceList(ctx context.Context) ([]Service, error)
	NodeList(ctx context.Context) ([]Node, error)
	JoinTokens(ctx context.Context) (JoinTokens, error)

	// ServiceInspect returns a read-only view of one service by name or ID.
	ServiceInspect(ctx context.Context, name string) (ServiceInspectResult, error)

	// ServiceTasks returns the task history for a service (one row per
	// `docker service ps` entry) — the crash/restart trail.
	ServiceTasks(ctx context.Context, serviceID string) ([]ServiceTask, error)

	// ServiceLogs returns up to tail lines of a service's stdout/stderr,
	// newest-last, demultiplexed with stream tags in chronological order.
	ServiceLogs(ctx context.Context, serviceID string, tail int) ([]LogLine, error)

	// ServiceRestart forces a rolling restart by bumping the service spec's
	// ForceUpdate counter (the daemon re-deploys running tasks in place).
	ServiceRestart(ctx context.Context, serviceID string) error

	// ServiceExec runs a fixed argv in a running task's container,
	// non-interactive. Returns a clear error when the service has no task
	// reachable from this node.
	ServiceExec(ctx context.Context, serviceID string, argv []string) (*ExecResult, error)

	SecretList(ctx context.Context, labelKey, labelValue string) ([]string, error)

	// VolumeList returns the names of volumes carrying the label
	// labelKey=labelValue.
	VolumeList(ctx context.Context, labelKey, labelValue string) ([]string, error)

	// StackSecretNames returns the deduplicated names of the secrets
	// mounted by the stack's services — what a stack delete must remove from
	// the swarm once the services are gone.
	StackSecretNames(ctx context.Context, stackName string) ([]string, error)

	SecretInspect(ctx context.Context, name string) (SecretInspectResult, error)

	ConfigList(ctx context.Context, labelKey, labelValue string) ([]string, error)

	ConfigInspect(ctx context.Context, name string) (ConfigInspectResult, error)

	Close() error
}

type Ping struct {
	APIVersion   string
	OSType       string
	Experimental bool
}

// ConfigInspectResult carries the labels and raw content of a runtime config.
// Data lets callers content-compare a rendered config against the currently
// deployed version without round-tripping through the raw API.
type ConfigInspectResult struct {
	Labels map[string]string
	Data   []byte
}

// SecretInspectResult carries the labels and raw content of a runtime secret.
type SecretInspectResult struct {
	Labels map[string]string
	Data   []byte
}

// Info is the subset of `docker info` pmcluster cares about.
type Info struct {
	Name                  string
	ServerVersion         string
	OperatingSystem       string
	Architecture          string
	NCPU                  int
	MemTotal              int64
	SwarmLocalNodeState   string
	SwarmControlAvailable bool
	SwarmManagers         int
	SwarmNodes            int
}

// NetworkSpec.Driver defaults to "overlay" if empty.
type NetworkSpec struct {
	Name       string
	Driver     string
	Attachable bool
}

// SecretSpec.Labels are applied so pmcluster-managed secrets can be
// distinguished from operator-created ones (cluster down --purge).
type SecretSpec struct {
	Name   string
	Data   []byte
	Labels map[string]string
}

// ConfigSpec is the config analogue of SecretSpec. Configs are non-sensitive
// bytes (rendered YAML); Swarm distributes them to every node automatically.
type ConfigSpec struct {
	Name   string
	Data   []byte
	Labels map[string]string
}

// Service is a view of a cluster service. The base fields power the
// cluster-up health check; the enriched fields (Stack, Image, Mode, UpdatedAt)
// power the service-ops read surface.
type Service struct {
	ID        string
	Name      string
	Stack     string // StackNamespaceLabel ("" when not stack-managed)
	Replicas  uint64
	Desired   uint64
	Image     string
	Mode      string // "replicated" | "global" | ""
	UpdatedAt int64
}

// ServiceInspectResult is a read-only snapshot of one cluster service.
type ServiceInspectResult struct {
	ID     string
	Name   string
	Image  string
	Labels map[string]string
}

// ServiceTask is one row of `docker service ps` — a task's lifecycle state.
type ServiceTask struct {
	TaskID     string
	Node       string
	Slot       int64
	State      string // running | failed | shutdown | rejected | ...
	Error      string // task error message, "" when running
	StartedAt  int64
	FinishedAt int64
}

// LogLine is one demultiplexed line of service output.
type LogLine struct {
	Stream string // "stdout" | "stderr"
	Line   string
}

// ExecResult is the buffered outcome of a non-interactive exec.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

type Node struct {
	ID            string
	Hostname      string
	Role          string
	Availability  string
	Status        string
	IsLeader      bool
	EngineVersion string
	Address       string
	CreatedAt     int64
	UpdatedAt     int64
}

type JoinTokens struct {
	Worker  string
	Manager string
}

// StackNamespaceLabel is the label attached to every resource (service,
// network, volume) created by `docker stack deploy` for a stack. VolumeList
// filters on it to find a stack's named volumes for teardown.
const StackNamespaceLabel = "com.docker.stack.namespace"
