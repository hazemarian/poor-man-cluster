// Package docker wraps the Docker Engine SDK behind a small interface so
// the rest of pmcluster can be tested with fakes instead of needing a
// real /var/run/docker.sock.
package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
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

	SecretList(ctx context.Context, labelKey, labelValue string) ([]string, error)

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

// Service is a minimal view of a Docker Swarm service — just the fields
// needed to check replica health in cluster up.
type Service struct {
	ID       string
	Name     string
	Replicas uint64
	Desired  uint64
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
		out = append(out, Service{
			ID:       s.ID,
			Name:     s.Spec.Name,
			Replicas: s.ServiceStatus.RunningTasks,
			Desired:  desired,
		})
	}
	return out, nil
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
