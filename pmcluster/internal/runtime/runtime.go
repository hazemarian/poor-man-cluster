// Package runtime is the neutral port contract pmcluster needs from a
// container orchestrator. Domain packages depend on THIS package, never on a
// concrete implementation (internal/docker is one adapter; a future k3s /
// Kubernetes backend would implement the same interface). The models here are
// deliberately small and runtime-agnostic — no Docker SDK types leak through.
package runtime

import (
	"context"
	"io"
	"time"
)

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

	// SwarmID returns the live Swarm cluster ID (the Raft cluster identity).
	// `cluster update` persists this and force-redeploys everything when it
	// changes — a wiped + re-initialised Swarm has a new ID even though the
	// local SQLite store (source of truth) survived.
	SwarmID(ctx context.Context) (string, error)

	// SetNodeLabel adds or replaces a label on a swarm node (used to mark
	// storage nodes so the backup agent can be constrained to them).
	SetNodeLabel(ctx context.Context, nodeID, key, value string) error

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

	// ServiceExecAttach starts an INTERACTIVE exec session (TTY, stdin
	// attached) in a running task's container and returns a bidirectional
	// stream. The caller owns the stream: it must Close it when done. The
	// returned error is clear when the service has no task reachable from
	// this node. rows/cols seed the initial terminal size (0 = 80x24).
	ServiceExecAttach(ctx context.Context, serviceID string, argv []string, rows, cols uint) (ExecStream, error)

	SecretList(ctx context.Context, labelKey, labelValue string) ([]string, error)

	// VolumeList returns the names of volumes carrying the label
	// labelKey=labelValue.
	VolumeList(ctx context.Context, labelKey, labelValue string) ([]string, error)

	// VolumeInspect returns one volume's definition. A stack volume is
	// declared as a local-driver bind, so Device carries the host path the
	// Swarm mounts (empty for a plain volume).
	VolumeInspect(ctx context.Context, name string) (Volume, error)

	// StackSecretNames returns the deduplicated names of the secrets
	// mounted by the stack's services — what a stack delete must remove from
	// the swarm once the services are gone.
	StackSecretNames(ctx context.Context, stackName string) ([]string, error)

	SecretInspect(ctx context.Context, name string) (SecretInspectResult, error)

	ConfigList(ctx context.Context, labelKey, labelValue string) ([]string, error)

	ConfigInspect(ctx context.Context, name string) (ConfigInspectResult, error)

	// Events streams swarm lifecycle events from `since` onward. The returned
	// event channel is closed when ctx is cancelled or the source stream ends;
	// errors (other than cancellation) are delivered on the error channel
	// before the event channel closes. Safe to call concurrently with every
	// other method on the client.
	Events(ctx context.Context, since time.Time) (<-chan Event, <-chan error)

	Close() error
}

// Ping reports the engine's API version and OS type.
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
	ID           string
	Name         string
	Stack        string // StackNamespaceLabel ("" when not stack-managed)
	Replicas     uint64
	Desired      uint64
	Image        string
	Mode         string // "replicated" | "global" | ""
	RunOnce      bool   // restart-policy "none": a one-shot job; 0 running replicas means it completed
	ImageCreated int64  // unix seconds the local image was created; 0 when the image is not cached locally
	UpdatedAt    int64
	UpdateState  string // swarm UpdateStatus.State: "updating" | "paused" | "completed" | "rollback..." | "" (no update in flight)
	UpdateError  string // the orchestrator's reason for pausing/rolling back ("" when none)
	Node         string // NodeLabel: the node hostname the placement pin targets ("" when unconstrained/role-based)
	// Platform reports a platform-managed service (edge, ingress, observability,
	// backup, SSO) — the ones stamped io.pmcluster.platform=true by the compose
	// writer for platform stacks. The console/CLI split these from customer app
	// services on this flag, never on a name or stack heuristic.
	Platform bool
	// Labels is the service's full spec label set (stack namespace, the
	// io.pmcluster.* markers, and RenderedHashLabel). `cluster update` reads
	// them to diff the live service against a fresh render.
	Labels map[string]string
}

// ServiceInspectResult is a read-only snapshot of one cluster service.
type ServiceInspectResult struct {
	ID     string
	Name   string
	Image  string
	Labels map[string]string
	// Mounts are the service's task-template mounts. A daemon uses them to
	// make sure the host directories a service binds exist locally before the
	// Swarm tries to start its task (a missing bind source is a hard failure).
	Mounts []Mount
}

// Mount is one container mount from a service's task template.
type Mount struct {
	Type   string // "bind" | "volume" | "tmpfs"
	Source string // host path (bind) or volume name (volume)
	Target string // container path
}

// Volume is one Docker volume's definition (the subset pmcluster uses).
type Volume struct {
	Name       string
	Driver     string
	Device     string // local-driver "device" option — the host bind path
	Mountpoint string // /var/lib/docker/volumes/<name>/_data
	// Bind reports a local-driver volume that mounts a host directory
	// (`o=bind, type=none`) — how pmcluster declares every stack volume. Its
	// Device is then inside the managed volume layout by construction.
	Bind bool
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

// ExecStream is a live, interactive exec session: a raw duplex byte stream
// to the process's TTY (stdin in, stdout/stderr out, no multiplexing).
// Resize requests a terminal size change; Wait blocks until the process
// exits and returns its exit code. Implementations must be safe for
// concurrent Read/Write/Resize.
type ExecStream interface {
	io.ReadWriteCloser
	// Resize asks the TTY to change dimensions (rows/cols). Best-effort:
	// implementations should tolerate engines that ignore it.
	Resize(ctx context.Context, rows, cols uint) error
	// Wait blocks until the exec session ends and returns its exit code.
	// A closed stream (Close) unblocks Wait with the session's final code.
	Wait(ctx context.Context) (int, error)
}

// Node describes one swarm node; IsLeader identifies the Raft leader the
// control-plane daemon follows.
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
	// Labels is the node's full spec label set, e.g. pmcluster.storage=true.
	// `cluster update` reads it to clear stale storage labels from nodes that
	// were removed from the storage_nodes setting.
	Labels map[string]string
}

// JoinTokens carries the worker and manager join tokens for new nodes.
type JoinTokens struct {
	Worker  string
	Manager string
}

// Event is a swarm lifecycle event the reconcile loop watches (service
// create/update/remove, task start/stop/die). It is a neutral, minimal
// projection of the orchestrator's event stream: Stack is derived from the
// com.docker.stack.namespace attribute, Service from com.docker.swarm.service.name,
// NodeID from com.docker.swarm.node.id.
type Event struct {
	Type      string // "service" | "task" (or pass-through orchestrator types)
	Action    string // orchestrator action string: create/update/remove/start/stop/die/kill
	Stack     string // com.docker.stack.namespace ("" when not stack-managed)
	Service   string // com.docker.swarm.service.name
	NodeID    string // com.docker.swarm.node.id
	Timestamp int64  // unix seconds the event was generated
}

// StackNamespaceLabel is the label attached to every resource (service,
// network, volume) created by `docker stack deploy` for a stack. VolumeList
// filters on it to find a stack's named volumes for teardown.
const StackNamespaceLabel = "com.docker.stack.namespace"

// StorageNodeLabel is the swarm NODE label marking a node as a storage node
// (pmcluster join --storage-node, or the cluster-up default leader). The
// backup agent is constrained to nodes carrying this label so backups only
// run on storage nodes.
const StorageNodeLabel = "pmcluster.storage"

// NodeLabel is stamped on every deployed service and names the node the
// service's placement constraint targets (a hostname pin, e.g. an
// auto-resolved storage node). It is empty when the service is unconstrained
// or role-based (node.role == manager/worker), so the UI can display where
// a service runs without re-querying the swarm.
const NodeLabel = "io.pmcluster.node"

// PlatformLabel is stamped on every service of a platform-managed stack
// (app.platform: true — edge, traefik, OpenObserve, otel-collector, backup,
// sso) by the compose writer. It is the discriminator the console/CLI use to
// keep platform services apart from customer app services. This is the
// canonical value: internal/manifest aliases it (labelPlatform) rather than
// repeating the literal, so there is one source of truth.
const PlatformLabel = "io.pmcluster.platform"

// RenderedHashLabel is stamped on every service of a rendered compose with the
// hash of that render (the platform render path sets it; see LoadComposeFile).
// `cluster update` compares the label on the LIVE Swarm services against a
// fresh render to detect drift — a manual `docker service update`, a
// half-applied deploy, or an upgrade that skipped a service. The stored
// rendered hash cannot see this: it only records what pmcluster last INTENDED
// to deploy, so a drifted service would otherwise stay drifted forever.
const RenderedHashLabel = "io.pmcluster.rendered_hash"
