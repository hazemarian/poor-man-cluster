// Package docker wraps the Docker Engine SDK behind a small interface so
// the rest of pmcluster can be tested with fakes instead of needing a
// real /var/run/docker.sock.
package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// Client is the contract pmcluster needs from a Docker daemon — a subset
// of the official SDK. *Exists/*Create methods collapse "not found" to
// (false, nil); idempotency is the caller's job. *Remove methods are
// idempotent (nil on missing).
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

	// StackSecretNames returns the deduplicated names of the Swarm secrets
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

// ConfigInspectResult carries the labels and raw content of a Docker config.
// Data lets callers content-compare a rendered config against the currently
// deployed version without round-tripping through Docker's raw API.
type ConfigInspectResult struct {
	Labels map[string]string
	Data   []byte
}

// SecretInspectResult carries the labels and raw content of a Docker secret.
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

// ConfigSpec is the Docker-config analogue of SecretSpec. Configs are
// non-sensitive bytes (rendered YAML); Swarm distributes them to every
// node automatically.
type ConfigSpec struct {
	Name   string
	Data   []byte
	Labels map[string]string
}

// Service is a view of a Docker Swarm service. The base fields power the
// cluster-up health check; the enriched fields (Stack, Image, Mode, UpdatedAt)
// power the service-ops read surface.
type Service struct {
	ID        string
	Name      string
	Stack     string // com.docker.stack.namespace label ("" when not stack-managed)
	Replicas  uint64
	Desired   uint64
	Image     string
	Mode      string // "replicated" | "global" | ""
	UpdatedAt int64
}

// ServiceInspectResult is a read-only snapshot of one swarm service.
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

type realClient struct {
	c *client.Client
}

// New returns a Client wired to the local Docker daemon (DOCKER_HOST or
// /var/run/docker.sock).
func New() (Client, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &realClient{c: c}, nil
}

func (r *realClient) Close() error { return r.c.Close() }

func (r *realClient) Ping(ctx context.Context) (Ping, error) {
	p, err := r.c.Ping(ctx)
	if err != nil {
		return Ping{}, fmt.Errorf("docker ping: %w", err)
	}
	return Ping{
		APIVersion:   p.APIVersion,
		OSType:       p.OSType,
		Experimental: p.Experimental,
	}, nil
}

func (r *realClient) Info(ctx context.Context) (Info, error) {
	i, err := r.c.Info(ctx)
	if err != nil {
		return Info{}, fmt.Errorf("docker info: %w", err)
	}
	return Info{
		Name:                  i.Name,
		ServerVersion:         i.ServerVersion,
		OperatingSystem:       i.OperatingSystem,
		Architecture:          i.Architecture,
		NCPU:                  i.NCPU,
		MemTotal:              i.MemTotal,
		SwarmLocalNodeState:   string(i.Swarm.LocalNodeState),
		SwarmControlAvailable: i.Swarm.ControlAvailable,
		SwarmManagers:         i.Swarm.Managers,
		SwarmNodes:            i.Swarm.Nodes,
	}, nil
}

func (r *realClient) NetworkExists(ctx context.Context, name string) (bool, error) {
	_, err := r.c.NetworkInspect(ctx, name, network.InspectOptions{})
	if err == nil {
		return true, nil
	}
	if cerrdefs.IsNotFound(err) || isNotFoundString(err) {
		return false, nil
	}
	return false, fmt.Errorf("network inspect %s: %w", name, err)
}

func (r *realClient) NetworkCreate(ctx context.Context, spec NetworkSpec) error {
	driver := spec.Driver
	if driver == "" {
		driver = "overlay"
	}
	_, err := r.c.NetworkCreate(ctx, spec.Name, network.CreateOptions{
		Driver:     driver,
		Attachable: spec.Attachable,
	})
	if err != nil {
		return fmt.Errorf("network create %s: %w", spec.Name, err)
	}
	return nil
}

func (r *realClient) SecretExists(ctx context.Context, name string) (bool, error) {
	_, _, err := r.c.SecretInspectWithRaw(ctx, name)
	if err == nil {
		return true, nil
	}
	if cerrdefs.IsNotFound(err) || isNotFoundString(err) {
		return false, nil
	}
	return false, fmt.Errorf("secret inspect %s: %w", name, err)
}

func (r *realClient) SecretInspect(ctx context.Context, name string) (SecretInspectResult, error) {
	sec, _, err := r.c.SecretInspectWithRaw(ctx, name)
	if err != nil {
		return SecretInspectResult{}, fmt.Errorf("secret inspect: %w", err)
	}
	return SecretInspectResult{Labels: sec.Spec.Labels, Data: sec.Spec.Data}, nil
}

func (r *realClient) SecretCreate(ctx context.Context, spec SecretSpec) error {
	annotations := swarm.Annotations{Name: spec.Name}
	if len(spec.Labels) > 0 {
		annotations.Labels = spec.Labels
	}
	_, err := r.c.SecretCreate(ctx, swarm.SecretSpec{
		Annotations: annotations,
		Data:        spec.Data,
	})
	if err != nil {
		return fmt.Errorf("secret create %s: %w", spec.Name, err)
	}
	return nil
}

func (r *realClient) ConfigExists(ctx context.Context, name string) (bool, error) {
	_, _, err := r.c.ConfigInspectWithRaw(ctx, name)
	if err == nil {
		return true, nil
	}
	if cerrdefs.IsNotFound(err) || isNotFoundString(err) {
		return false, nil
	}
	return false, fmt.Errorf("config inspect %s: %w", name, err)
}

func (r *realClient) ConfigCreate(ctx context.Context, spec ConfigSpec) error {
	annotations := swarm.Annotations{Name: spec.Name}
	if len(spec.Labels) > 0 {
		annotations.Labels = spec.Labels
	}
	_, err := r.c.ConfigCreate(ctx, swarm.ConfigSpec{
		Annotations: annotations,
		Data:        spec.Data,
	})
	if err != nil {
		return fmt.Errorf("config create %s: %w", spec.Name, err)
	}
	return nil
}

// idempotentRemove maps "not found" to nil so teardown loops don't bail.
func idempotentRemove(err error, kind, name string) error {
	if err == nil {
		return nil
	}
	if cerrdefs.IsNotFound(err) || isNotFoundString(err) {
		return nil
	}
	return fmt.Errorf("%s remove %s: %w", kind, name, err)
}

func (r *realClient) SecretRemove(ctx context.Context, name string) error {
	return idempotentRemove(r.c.SecretRemove(ctx, name), "secret", name)
}

func (r *realClient) ConfigRemove(ctx context.Context, name string) error {
	return idempotentRemove(r.c.ConfigRemove(ctx, name), "config", name)
}

func (r *realClient) NetworkRemove(ctx context.Context, name string) error {
	return idempotentRemove(r.c.NetworkRemove(ctx, name), "network", name)
}

func (r *realClient) VolumeRemove(ctx context.Context, name string) error {
	return idempotentRemove(r.c.VolumeRemove(ctx, name, true), "volume", name)
}

// StackNamespaceLabel is the Docker label Docker attaches to every resource
// (service, network, volume) created by `docker stack deploy` for a stack.
// VolumeList filters on it to find a stack's named volumes for teardown.
const StackNamespaceLabel = "com.docker.stack.namespace"

func (r *realClient) VolumeList(ctx context.Context, labelKey, labelValue string) ([]string, error) {
	vols, err := r.c.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", labelKey+"="+labelValue)),
	})
	if err != nil {
		return nil, fmt.Errorf("docker volume ls: %w", err)
	}
	var out []string
	for _, v := range vols.Volumes {
		out = append(out, v.Name)
	}
	return out, nil
}

func (r *realClient) StackSecretNames(ctx context.Context, stackName string) ([]string, error) {
	svcs, err := r.c.ServiceList(ctx, swarm.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("label", StackNamespaceLabel+"="+stackName)),
	})
	if err != nil {
		return nil, fmt.Errorf("docker service ls %s: %w", stackName, err)
	}
	seen := make(map[string]bool)
	var out []string
	for _, s := range svcs {
		if s.Spec.TaskTemplate.ContainerSpec == nil {
			continue
		}
		for _, sec := range s.Spec.TaskTemplate.ContainerSpec.Secrets {
			if !seen[sec.SecretName] {
				seen[sec.SecretName] = true
				out = append(out, sec.SecretName)
			}
		}
	}
	return out, nil
}

func (r *realClient) ServiceList(ctx context.Context) ([]Service, error) {
	svcs, err := r.c.ServiceList(ctx, swarm.ServiceListOptions{Status: true})
	if err != nil {
		return nil, fmt.Errorf("docker service ls: %w", err)
	}
	out := make([]Service, 0, len(svcs))
	for _, s := range svcs {
		spec := s.Spec
		desired := uint64(0)
		if spec.Mode.Replicated != nil && spec.Mode.Replicated.Replicas != nil {
			desired = *spec.Mode.Replicated.Replicas
		}

		if spec.Mode.Global != nil {
			desired = s.ServiceStatus.RunningTasks
		}
		image := ""
		if c := spec.TaskTemplate.ContainerSpec; c != nil {
			image = c.Image
		}
		mode := ""
		switch {
		case spec.Mode.Replicated != nil:
			mode = "replicated"
		case spec.Mode.Global != nil:
			mode = "global"
		}
		out = append(out, Service{
			ID:        s.ID,
			Name:      s.Spec.Name,
			Stack:     s.Spec.Labels[StackNamespaceLabel],
			Replicas:  s.ServiceStatus.RunningTasks,
			Desired:   desired,
			Image:     image,
			Mode:      mode,
			UpdatedAt: s.UpdatedAt.Unix(),
		})
	}
	return out, nil
}

// ServiceInspect resolves a service by name or ID to a read-only snapshot.
func (r *realClient) ServiceInspect(ctx context.Context, name string) (ServiceInspectResult, error) {
	svc, _, err := r.c.ServiceInspectWithRaw(ctx, name, swarm.ServiceInspectOptions{})
	if err != nil {
		return ServiceInspectResult{}, fmt.Errorf("service inspect %s: %w", name, err)
	}
	labels := svc.Spec.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	image := ""
	if c := svc.Spec.TaskTemplate.ContainerSpec; c != nil {
		image = c.Image
	}
	return ServiceInspectResult{
		ID:     svc.ID,
		Name:   svc.Spec.Name,
		Image:  image,
		Labels: labels,
	}, nil
}

// ServiceTasks returns the task history for a service, newest last. The node
// hostname is resolved via the current node list.
func (r *realClient) ServiceTasks(ctx context.Context, serviceID string) ([]ServiceTask, error) {
	svc, err := r.ServiceInspect(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	tasks, err := r.c.TaskList(ctx, swarm.TaskListOptions{
		Filters: filters.NewArgs(filters.Arg("service", svc.ID)),
	})
	if err != nil {
		return nil, fmt.Errorf("task list %s: %w", svc.Name, err)
	}
	// Map node IDs to hostnames once.
	hosts := map[string]string{}
	if nodes, nerr := r.NodeList(ctx); nerr == nil {
		for _, n := range nodes {
			hosts[n.ID] = n.Hostname
		}
	}
	out := make([]ServiceTask, 0, len(tasks))
	for _, t := range tasks {
		started, finished := int64(0), int64(0)
		if !t.Status.Timestamp.IsZero() {
			started = t.Status.Timestamp.Unix()
		}
		msg := t.Status.Message
		if msg == "" {
			msg = t.Status.Err
		}
		out = append(out, ServiceTask{
			TaskID:     t.ID,
			Node:       hosts[t.NodeID],
			Slot:       int64(t.Slot),
			State:      string(t.Status.State),
			Error:      msg,
			StartedAt:  started,
			FinishedAt: finished,
		})
	}
	return out, nil
}

// ServiceLogs tails a service's combined stdout/stderr, preserving stream
// tags and chronological order. Tail is clamped to [1, 2000].
func (r *realClient) ServiceLogs(ctx context.Context, serviceID string, tail int) ([]LogLine, error) {
	if tail < 1 {
		tail = 1
	}
	if tail > 2000 {
		tail = 2000
	}
	svc, _, err := r.c.ServiceInspectWithRaw(ctx, serviceID, swarm.ServiceInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("service inspect %s: %w", serviceID, err)
	}
	tty := false
	if c := svc.Spec.TaskTemplate.ContainerSpec; c != nil {
		tty = c.TTY
	}

	rc, err := r.c.ServiceLogs(ctx, svc.ID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(tail),
	})
	if err != nil {
		return nil, fmt.Errorf("service logs %s: %w", svc.Spec.Name, err)
	}
	defer rc.Close()

	if tty {
		// TTY services stream raw (un-multiplexed) stdout.
		raw, err := io.ReadAll(rc)
		if err != nil {
			return nil, fmt.Errorf("service logs %s: %w", svc.Spec.Name, err)
		}
		return splitLines("stdout", string(raw)), nil
	}

	// Multiplexed stream: parse 8-byte headers, keep interleaved order.
	return demuxLines(rc, tail)
}

// demuxLines reads a Docker multiplexed stream and returns up to tail lines
// tagged with their stream, preserving the interleaved order. The header is
// [streamType(1) pad(3) size(4, big-endian)] followed by size payload bytes.
func demuxLines(r io.Reader, tail int) ([]LogLine, error) {
	all := make([]LogLine, 0, tail)
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("read log stream: %w", err)
		}
		size := int(hdr[4])<<24 | int(hdr[5])<<16 | int(hdr[6])<<8 | int(hdr[7])
		if size <= 0 || size > 16<<20 {
			break
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, fmt.Errorf("read log frame: %w", err)
		}
		stream := "stdout"
		if hdr[0] == 2 {
			stream = "stderr"
		}
		all = append(all, splitLines(stream, string(buf))...)
		if len(all) > tail {
			all = all[len(all)-tail:]
		}
	}
	return all, nil
}

// splitLines splits raw output into non-empty lines tagged with their stream.
func splitLines(stream, s string) []LogLine {
	var out []LogLine
	for _, ln := range strings.Split(s, "\n") {
		if ln == "" {
			continue
		}
		out = append(out, LogLine{Stream: stream, Line: ln})
	}
	return out
}

// ServiceRestart forces a rolling restart by bumping the spec's ForceUpdate
// counter and pushing the spec with its current version.
func (r *realClient) ServiceRestart(ctx context.Context, serviceID string) error {
	svc, _, err := r.c.ServiceInspectWithRaw(ctx, serviceID, swarm.ServiceInspectOptions{})
	if err != nil {
		return fmt.Errorf("service inspect %s: %w", serviceID, err)
	}
	svc.Spec.TaskTemplate.ForceUpdate++
	if _, err := r.c.ServiceUpdate(ctx, svc.ID, svc.Version, svc.Spec, swarm.ServiceUpdateOptions{}); err != nil {
		return fmt.Errorf("service restart %s: %w", svc.Spec.Name, err)
	}
	return nil
}

// ServiceExec runs a fixed argv non-interactively in the first running task
// whose container is reachable from this node. The daemon only has its own
// docker socket, so tasks running on other swarm nodes cannot be exec'd here;
// a clear error is returned in that case.
func (r *realClient) ServiceExec(ctx context.Context, serviceID string, argv []string) (*ExecResult, error) {
	svc, err := r.ServiceInspect(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	tasks, err := r.c.TaskList(ctx, swarm.TaskListOptions{
		Filters: filters.NewArgs(filters.Arg("service", svc.ID)),
	})
	if err != nil {
		return nil, fmt.Errorf("task list %s: %w", svc.Name, err)
	}

	var lastErr error
	for _, t := range tasks {
		if t.Status.State != swarm.TaskStateRunning || t.Status.ContainerStatus == nil {
			continue
		}
		cid := t.Status.ContainerStatus.ContainerID
		if cid == "" {
			continue
		}
		res, err := r.execInContainer(ctx, cid, argv)
		if err != nil {
			lastErr = err
			continue
		}
		return res, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("service %s: no running task reachable on this node: %w", svc.Name, lastErr)
	}
	return nil, fmt.Errorf("service %s: no running task on this node (tasks run on other nodes — use ssh + docker exec)", svc.Name)
}

// execInContainer creates + attaches + inspects a non-interactive exec.
func (r *realClient) execInContainer(ctx context.Context, containerID string, argv []string) (*ExecResult, error) {
	cfg := container.ExecOptions{
		Cmd:          argv,
		AttachStdin:  false,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          false,
	}
	created, err := r.c.ContainerExecCreate(ctx, containerID, cfg)
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}
	hij, err := r.c.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("exec attach: %w", err)
	}
	defer hij.Close()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, hij.Reader); err != nil {
		return nil, fmt.Errorf("exec read: %w", err)
	}

	insp, err := r.c.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return nil, fmt.Errorf("exec inspect: %w", err)
	}
	return &ExecResult{
		ExitCode: insp.ExitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

func (r *realClient) NodeList(ctx context.Context) ([]Node, error) {
	nodes, err := r.c.NodeList(ctx, swarm.NodeListOptions{})
	if err != nil {
		return nil, fmt.Errorf("docker node ls: %w", err)
	}
	out := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		var addr string
		if n.ManagerStatus != nil {
			addr = n.ManagerStatus.Addr
		}
		out = append(out, Node{
			ID:            n.ID,
			Hostname:      n.Description.Hostname,
			Role:          string(n.Spec.Role),
			Availability:  string(n.Spec.Availability),
			Status:        string(n.Status.State),
			IsLeader:      n.ManagerStatus != nil && n.ManagerStatus.Leader,
			EngineVersion: n.Description.Engine.EngineVersion,
			Address:       addr,
			CreatedAt:     n.CreatedAt.Unix(),
			UpdatedAt:     n.UpdatedAt.Unix(),
		})
	}
	return out, nil
}

func (r *realClient) JoinTokens(ctx context.Context) (JoinTokens, error) {
	sw, err := r.c.SwarmInspect(ctx)
	if err != nil {
		return JoinTokens{}, fmt.Errorf("docker swarm inspect: %w", err)
	}
	return JoinTokens{
		Worker:  sw.JoinTokens.Worker,
		Manager: sw.JoinTokens.Manager,
	}, nil
}

func (r *realClient) SecretList(ctx context.Context, labelKey, labelValue string) ([]string, error) {
	opts := swarm.SecretListOptions{}
	if labelKey != "" {
		opts.Filters = filters.NewArgs()
		opts.Filters.Add("label", labelKey+"="+labelValue)
	}
	secrets, err := r.c.SecretList(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("secret list: %w", err)
	}
	names := make([]string, 0, len(secrets))
	for _, s := range secrets {
		names = append(names, s.Spec.Name)
	}
	return names, nil
}

func (r *realClient) ConfigList(ctx context.Context, labelKey, labelValue string) ([]string, error) {
	opts := swarm.ConfigListOptions{}
	if labelKey != "" {
		opts.Filters = filters.NewArgs()
		opts.Filters.Add("label", labelKey+"="+labelValue)
	}
	configs, err := r.c.ConfigList(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("config list: %w", err)
	}
	names := make([]string, 0, len(configs))
	for _, c := range configs {
		names = append(names, c.Spec.Name)
	}
	return names, nil
}

func (r *realClient) ConfigInspect(ctx context.Context, name string) (ConfigInspectResult, error) {
	cfg, _, err := r.c.ConfigInspectWithRaw(ctx, name)
	if err != nil {
		return ConfigInspectResult{}, fmt.Errorf("config inspect: %w", err)
	}
	return ConfigInspectResult{Labels: cfg.Spec.Labels, Data: cfg.Spec.Data}, nil
}

// isNotFoundString is a fallback for older daemons whose error doesn't
// satisfy errdefs.IsNotFound. Belt-and-braces.
func isNotFoundString(err error) bool {
	if err == nil {
		return false
	}
	var msg []byte
	msg = append(msg, err.Error()...)
	return bytes.Contains(bytes.ToLower(msg), []byte("not found")) ||
		bytes.Contains(bytes.ToLower(msg), []byte("no such")) ||
		errors.Is(err, errNotFound)
}

var errNotFound = errors.New("not found")
