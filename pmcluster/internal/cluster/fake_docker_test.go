package cluster

import (
	"context"
	"errors"
	"fmt"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
)

// fakeDocker is a minimal in-process implementation of docker.Client for
// cluster-package tests. It tracks state in plain maps/slices and can be
// pre-populated to simulate "already exists" conditions.
//
// Intentionally separate from the one in internal/docker (which is private to
// that package) so each package controls its own test doubles.
type fakeDocker struct {
	// Controls for Ping / Info.
	pingErr error
	infoErr error
	info    docker.Info

	// In-memory resources, keyed by name.
	networks map[string]docker.NetworkSpec
	secrets  map[string]docker.SecretSpec
	configs  map[string]docker.ConfigSpec
	services map[string]docker.Service

	// Removal tracking — appended to on each Remove call.
	removedSecrets  []string
	removedConfigs  []string
	removedNetworks []string
	removedVolumes  []string

	// Injected error overrides for specific operations.
	networkExistsErr error
	networkCreateErr error
	secretExistsErr  error
	secretCreateErr  error
	configExistsErr  error
	configCreateErr  error

	// secretRemoveErr maps secret name → error to inject on SecretRemove.
	// Used by Rotate tests to simulate "secret in use" failures.
	secretRemoveErr map[string]error
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		networks: make(map[string]docker.NetworkSpec),
		secrets:  make(map[string]docker.SecretSpec),
		configs:  make(map[string]docker.ConfigSpec),
		services: make(map[string]docker.Service),
	}
}

// goodSwarmInfo returns an Info that satisfies all preflight checks.
func goodSwarmInfo() docker.Info {
	return docker.Info{
		Name:                  "test-node",
		ServerVersion:         "27.0.0",
		SwarmLocalNodeState:   "active",
		SwarmControlAvailable: true,
		SwarmManagers:         1,
		SwarmNodes:            1,
	}
}

func (f *fakeDocker) Ping(_ context.Context) (docker.Ping, error) {
	return docker.Ping{APIVersion: "1.45", OSType: "linux"}, f.pingErr
}

func (f *fakeDocker) Info(_ context.Context) (docker.Info, error) {
	return f.info, f.infoErr
}

func (f *fakeDocker) NetworkExists(_ context.Context, name string) (bool, error) {
	if f.networkExistsErr != nil {
		return false, f.networkExistsErr
	}
	_, ok := f.networks[name]
	return ok, nil
}

func (f *fakeDocker) NetworkCreate(_ context.Context, spec docker.NetworkSpec) error {
	if f.networkCreateErr != nil {
		return f.networkCreateErr
	}
	f.networks[spec.Name] = spec
	return nil
}

func (f *fakeDocker) SecretExists(_ context.Context, name string) (bool, error) {
	if f.secretExistsErr != nil {
		return false, f.secretExistsErr
	}
	_, ok := f.secrets[name]
	return ok, nil
}

func (f *fakeDocker) SecretCreate(_ context.Context, spec docker.SecretSpec) error {
	if f.secretCreateErr != nil {
		return f.secretCreateErr
	}
	f.secrets[spec.Name] = spec
	return nil
}

func (f *fakeDocker) ConfigExists(_ context.Context, name string) (bool, error) {
	if f.configExistsErr != nil {
		return false, f.configExistsErr
	}
	_, ok := f.configs[name]
	return ok, nil
}

func (f *fakeDocker) ConfigCreate(_ context.Context, spec docker.ConfigSpec) error {
	if f.configCreateErr != nil {
		return f.configCreateErr
	}
	f.configs[spec.Name] = spec
	return nil
}

func (f *fakeDocker) ServiceList(_ context.Context) ([]docker.Service, error) {
	out := make([]docker.Service, 0, len(f.services))
	for _, s := range f.services {
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeDocker) SecretRemove(_ context.Context, name string) error {
	if f.secretRemoveErr != nil {
		if err, ok := f.secretRemoveErr[name]; ok {
			return err
		}
	}
	f.removedSecrets = append(f.removedSecrets, name)
	delete(f.secrets, name)
	return nil
}

func (f *fakeDocker) ConfigRemove(_ context.Context, name string) error {
	f.removedConfigs = append(f.removedConfigs, name)
	delete(f.configs, name)
	return nil
}

func (f *fakeDocker) NetworkRemove(_ context.Context, name string) error {
	f.removedNetworks = append(f.removedNetworks, name)
	delete(f.networks, name)
	return nil
}

func (f *fakeDocker) VolumeRemove(_ context.Context, name string) error {
	f.removedVolumes = append(f.removedVolumes, name)
	return nil
}

func (f *fakeDocker) SecretList(_ context.Context, _, _ string) ([]string, error) {
	names := make([]string, 0, len(f.secrets))
	for n := range f.secrets {
		names = append(names, n)
	}
	return names, nil
}

func (f *fakeDocker) SecretInspect(_ context.Context, name string) (docker.SecretInspectResult, error) {
	if s, ok := f.secrets[name]; ok {
		return docker.SecretInspectResult{Labels: s.Labels, Data: s.Data}, nil
	}
	return docker.SecretInspectResult{}, fmt.Errorf("secret %q not found", name)
}

func (f *fakeDocker) ConfigList(_ context.Context, labelKey, labelValue string) ([]string, error) {
	names := make([]string, 0, len(f.configs))
	for n, c := range f.configs {
		if labelKey != "" && c.Labels[labelKey] != labelValue {
			continue
		}
		names = append(names, n)
	}
	return names, nil
}

func (f *fakeDocker) ConfigInspect(_ context.Context, name string) (docker.ConfigInspectResult, error) {
	if c, ok := f.configs[name]; ok {
		return docker.ConfigInspectResult{Labels: c.Labels, Data: c.Data}, nil
	}
	return docker.ConfigInspectResult{}, fmt.Errorf("config %q not found", name)
}

func (f *fakeDocker) NodeList(_ context.Context) ([]docker.Node, error) { return nil, nil }
func (f *fakeDocker) JoinTokens(_ context.Context) (docker.JoinTokens, error) {
	return docker.JoinTokens{}, nil
}

func (f *fakeDocker) Close() error { return nil }

// Compile-time assertion.
var _ docker.Client = (*fakeDocker)(nil)

// recordingDeployer captures deploy/remove calls for order + count assertions.
type recordingDeployer struct {
	deployedStacks []deployRecord
	removedStacks  []string
	forceUpdated   []string
	removeErr      error
}

type deployRecord struct {
	Name    string
	YAMLLen int
	YAML    string
}

func (r *recordingDeployer) DeployStack(_ context.Context, name string, composeYAML []byte) error {
	r.deployedStacks = append(r.deployedStacks, deployRecord{Name: name, YAMLLen: len(composeYAML), YAML: string(composeYAML)})
	return nil
}

func (r *recordingDeployer) RemoveStack(_ context.Context, name string) error {
	if r.removeErr != nil {
		return r.removeErr
	}
	r.removedStacks = append(r.removedStacks, name)
	return nil
}

func (r *recordingDeployer) ForceUpdateService(_ context.Context, fullName string) error {
	r.forceUpdated = append(r.forceUpdated, fullName)
	return nil
}

func (r *recordingDeployer) PruneStaleContainers(_ context.Context, _ string, _ string) error {
	return nil
}

// Compile-time assertion.
var _ StackDeployer = (*recordingDeployer)(nil)

// errSentinel is a simple non-nil error for tests that just need "some error".
var errSentinel = errors.New("fake docker: injected error")
