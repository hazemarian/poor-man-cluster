// Package docker adapts the Docker Engine SDK to the neutral runtime.Client
// port (internal/runtime). Everything pmcluster needs from the orchestrator is
// declared in that port package; this package only knows how to talk to Docker
// Swarm. A future k3s / Kubernetes backend implements the same port.
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

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/runtime"
)

type realClient struct {
	c *client.Client
}

// New returns a runtime.Client wired to the local Docker daemon (DOCKER_HOST
// or /var/run/docker.sock).
func New() (runtime.Client, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &realClient{c: c}, nil
}

func (r *realClient) Close() error { return r.c.Close() }

func (r *realClient) Ping(ctx context.Context) (runtime.Ping, error) {
	p, err := r.c.Ping(ctx)
	if err != nil {
		return runtime.Ping{}, fmt.Errorf("docker ping: %w", err)
	}
	return runtime.Ping{
		APIVersion:   p.APIVersion,
		OSType:       p.OSType,
		Experimental: p.Experimental,
	}, nil
}

func (r *realClient) Info(ctx context.Context) (runtime.Info, error) {
	i, err := r.c.Info(ctx)
	if err != nil {
		return runtime.Info{}, fmt.Errorf("docker info: %w", err)
	}
	return runtime.Info{
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

func (r *realClient) NetworkCreate(ctx context.Context, spec runtime.NetworkSpec) error {
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

func (r *realClient) SecretInspect(ctx context.Context, name string) (runtime.SecretInspectResult, error) {
	sec, _, err := r.c.SecretInspectWithRaw(ctx, name)
	if err != nil {
		return runtime.SecretInspectResult{}, fmt.Errorf("secret inspect: %w", err)
	}
	return runtime.SecretInspectResult{Labels: sec.Spec.Labels, Data: sec.Spec.Data}, nil
}

func (r *realClient) SecretCreate(ctx context.Context, spec runtime.SecretSpec) error {
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

func (r *realClient) ConfigCreate(ctx context.Context, spec runtime.ConfigSpec) error {
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

// runtime.StackNamespaceLabel (com.docker.stack.namespace) is the label
// Docker attaches to every resource created by `docker stack deploy` for a
// stack; VolumeList filters on it to find a stack's named volumes for teardown.
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
		Filters: filters.NewArgs(filters.Arg("label", runtime.StackNamespaceLabel+"="+stackName)),
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

func (r *realClient) ServiceList(ctx context.Context) ([]runtime.Service, error) {
	svcs, err := r.c.ServiceList(ctx, swarm.ServiceListOptions{Status: true})
	if err != nil {
		return nil, fmt.Errorf("docker service ls: %w", err)
	}
	out := make([]runtime.Service, 0, len(svcs))
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
		out = append(out, runtime.Service{
			ID:        s.ID,
			Name:      s.Spec.Name,
			Stack:     s.Spec.Labels[runtime.StackNamespaceLabel],
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
func (r *realClient) ServiceInspect(ctx context.Context, name string) (runtime.ServiceInspectResult, error) {
	svc, _, err := r.c.ServiceInspectWithRaw(ctx, name, swarm.ServiceInspectOptions{})
	if err != nil {
		return runtime.ServiceInspectResult{}, fmt.Errorf("service inspect %s: %w", name, err)
	}
	labels := svc.Spec.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	image := ""
	if c := svc.Spec.TaskTemplate.ContainerSpec; c != nil {
		image = c.Image
	}
	return runtime.ServiceInspectResult{
		ID:     svc.ID,
		Name:   svc.Spec.Name,
		Image:  image,
		Labels: labels,
	}, nil
}

// ServiceTasks returns the task history for a service, newest last. The node
// hostname is resolved via the current node list.
func (r *realClient) ServiceTasks(ctx context.Context, serviceID string) ([]runtime.ServiceTask, error) {
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
	out := make([]runtime.ServiceTask, 0, len(tasks))
	for _, t := range tasks {
		started, finished := int64(0), int64(0)
		if !t.Status.Timestamp.IsZero() {
			started = t.Status.Timestamp.Unix()
		}
		msg := t.Status.Message
		if msg == "" {
			msg = t.Status.Err
		}
		out = append(out, runtime.ServiceTask{
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
func (r *realClient) ServiceLogs(ctx context.Context, serviceID string, tail int) ([]runtime.LogLine, error) {
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
func demuxLines(r io.Reader, tail int) ([]runtime.LogLine, error) {
	all := make([]runtime.LogLine, 0, tail)
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
func splitLines(stream, s string) []runtime.LogLine {
	var out []runtime.LogLine
	for _, ln := range strings.Split(s, "\n") {
		if ln == "" {
			continue
		}
		out = append(out, runtime.LogLine{Stream: stream, Line: ln})
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
func (r *realClient) ServiceExec(ctx context.Context, serviceID string, argv []string) (*runtime.ExecResult, error) {
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
func (r *realClient) execInContainer(ctx context.Context, containerID string, argv []string) (*runtime.ExecResult, error) {
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
	return &runtime.ExecResult{
		ExitCode: insp.ExitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

func (r *realClient) NodeList(ctx context.Context) ([]runtime.Node, error) {
	nodes, err := r.c.NodeList(ctx, swarm.NodeListOptions{})
	if err != nil {
		return nil, fmt.Errorf("docker node ls: %w", err)
	}
	out := make([]runtime.Node, 0, len(nodes))
	for _, n := range nodes {
		var addr string
		if n.ManagerStatus != nil {
			addr = n.ManagerStatus.Addr
		}
		out = append(out, runtime.Node{
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

func (r *realClient) JoinTokens(ctx context.Context) (runtime.JoinTokens, error) {
	sw, err := r.c.SwarmInspect(ctx)
	if err != nil {
		return runtime.JoinTokens{}, fmt.Errorf("docker swarm inspect: %w", err)
	}
	return runtime.JoinTokens{
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

func (r *realClient) ConfigInspect(ctx context.Context, name string) (runtime.ConfigInspectResult, error) {
	cfg, _, err := r.c.ConfigInspectWithRaw(ctx, name)
	if err != nil {
		return runtime.ConfigInspectResult{}, fmt.Errorf("config inspect: %w", err)
	}
	return runtime.ConfigInspectResult{Labels: cfg.Spec.Labels, Data: cfg.Spec.Data}, nil
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
