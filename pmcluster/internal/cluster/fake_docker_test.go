package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// fakeDocker is a minimal in-process implementation of runtime.Client for
// cluster-package tests. It tracks state in plain maps/slices and can be
// pre-populated to simulate "already exists" conditions.
//
// Intentionally separate from the one in internal/docker (which is private to
// that package) so each package controls its own test doubles.
type fakeDocker struct {
	// Controls for Ping / Info.
	pingErr error
	infoErr error
	info    runtime.Info

	// In-memory resources, keyed by name.
	networks map[string]runtime.NetworkSpec
	secrets  map[string]runtime.SecretSpec
	configs  map[string]runtime.ConfigSpec
	services map[string]runtime.Service
	nodes    []runtime.Node

	// swarmID is the Swarm cluster ID SwarmID reports ("" = unset). Tests
	// mutate it to simulate a wiped + re-initialised Swarm.
	swarmID string

	// Removal tracking — appended to on each Remove call.
	removedSecrets  []string
	removedConfigs  []string
	removedNetworks []string
	removedVolumes  []string

	// Label tracking — appended to on each SetNodeLabel call.
	nodeLabels []string

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
		networks: make(map[string]runtime.NetworkSpec),
		secrets:  make(map[string]runtime.SecretSpec),
		configs:  make(map[string]runtime.ConfigSpec),
		services: make(map[string]runtime.Service),
	}
}

// goodSwarmInfo returns an Info that satisfies all preflight checks.
func goodSwarmInfo() runtime.Info {
	return runtime.Info{
		Name:                  "test-node",
		ServerVersion:         "27.0.0",
		SwarmLocalNodeState:   "active",
		SwarmControlAvailable: true,
		SwarmManagers:         1,
		SwarmNodes:            1,
	}
}

func (f *fakeDocker) Ping(_ context.Context) (runtime.Ping, error) {
	return runtime.Ping{APIVersion: "1.45", OSType: "linux"}, f.pingErr
}

func (f *fakeDocker) Info(_ context.Context) (runtime.Info, error) {
	return f.info, f.infoErr
}

func (f *fakeDocker) NetworkExists(_ context.Context, name string) (bool, error) {
	if f.networkExistsErr != nil {
		return false, f.networkExistsErr
	}
	_, ok := f.networks[name]
	return ok, nil
}

func (f *fakeDocker) NetworkCreate(_ context.Context, spec runtime.NetworkSpec) error {
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

func (f *fakeDocker) SecretCreate(_ context.Context, spec runtime.SecretSpec) error {
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

func (f *fakeDocker) ConfigCreate(_ context.Context, spec runtime.ConfigSpec) error {
	if f.configCreateErr != nil {
		return f.configCreateErr
	}
	f.configs[spec.Name] = spec
	return nil
}

// renderedServices is the service set the bundled stacks render on a cluster
// with the in-cluster backup store enabled — i.e. the swarm shape the
// reconcile compares a fresh render against. It is a superset of
// bundledServices (which drives health checks and lists only the services
// that are always present, so it cannot include the conditional store).
func renderedServices() []string {
	return append(append([]string{}, bundledServices...),
		"backup_control-plane-backup",
		"backup_seaweedfs",
	)
}

// seedHealthySwarm marks every rendered bundled service as present, so a
// baseline update against this fixture is a no-op (BUG-017's drift check
// compares the live service set against the render's).
func seedHealthySwarm(f *fakeDocker) {
	for _, name := range renderedServices() {
		f.services[name] = healthyService(name)
	}
}

// renderedHashLabel parses a rendered compose and returns the
// io.pmcluster.rendered_hash label value stamped on its services ("" when the
// render carries no label).
func renderedHashLabel(rendered []byte) string {
	var doc struct {
		Services map[string]struct {
			Deploy struct {
				Labels map[string]string `json:"labels"`
			} `json:"deploy"`
		} `json:"services"`
	}
	if err := yaml.Unmarshal(rendered, &doc); err != nil {
		return ""
	}
	for _, svc := range doc.Services {
		if h := svc.Deploy.Labels[runtime.RenderedHashLabel]; h != "" {
			return h
		}
	}
	return ""
}

// stampRenderedHashLabels reads the stored rendered content for every platform
// stack config and stamps the rendered-hash label on the matching live fake
// services — the shape a real swarm is in after a successful deploy. Without
// it, `cluster update`'s drift check sees zero labelled live services and
// (correctly, per the all-unlabelled rule) treats every platform stack as
// drift, redeploying on every no-op update.
//
// This helper exists for TESTS ONLY: it reconstructs the post-deploy swarm
// state the fakeDocker does not build on its own (its DeployStack is a
// recording no-op). It does not weaken the production rule — the production
// rule is unchanged; the fixture is made to match reality.
func stampRenderedHashLabels(t *testing.T, s *store.Store, f *fakeDocker) {
	t.Helper()
	ctx := context.Background()
	for _, st := range []stackName{StackObservability, StackInfra, StackEdge, StackBackup, StackSSO} {
		row, err := s.GetConfig(ctx, string(st)+"-stack")
		if err != nil {
			if errors.Is(err, store.ErrConfigNotFound) {
				continue // e.g. sso when SSO is disabled
			}
			t.Fatalf("read %s-stack: %v", st, err)
		}
		hash := renderedHashLabel([]byte(row.RenderedContent))
		if hash == "" {
			continue
		}
		prefix := string(st) + "_"
		for name, svc := range f.services {
			if strings.HasPrefix(name, prefix) {
				svc.Labels = map[string]string{runtime.RenderedHashLabel: hash}
				f.services[name] = svc
			}
		}
	}
}

func (f *fakeDocker) ServiceList(_ context.Context) ([]runtime.Service, error) {
	out := make([]runtime.Service, 0, len(f.services))
	for _, s := range f.services {
		out = append(out, s)
	}
	return out, nil
}

// ServiceInspect/Tasks/Logs/Restart/Exec are unused by the cluster package;
// stub them so this fake keeps satisfying the growing interface.
func (f *fakeDocker) ServiceInspect(_ context.Context, _ string) (runtime.ServiceInspectResult, error) {
	return runtime.ServiceInspectResult{}, nil
}
func (f *fakeDocker) ServiceTasks(_ context.Context, _ string) ([]runtime.ServiceTask, error) {
	return nil, nil
}
func (f *fakeDocker) ServiceLogs(_ context.Context, _ string, _ int) ([]runtime.LogLine, error) {
	return nil, nil
}
func (f *fakeDocker) ServiceRestart(_ context.Context, _ string) error { return nil }
func (f *fakeDocker) ServiceExec(_ context.Context, _ string, _ []string) (*runtime.ExecResult, error) {
	return nil, nil
}
func (f *fakeDocker) ServiceExecAttach(_ context.Context, _ string, _ []string, _, _ uint) (runtime.ExecStream, error) {
	return nil, nil
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

func (f *fakeDocker) VolumeList(_ context.Context, _, _ string) ([]string, error) {
	return nil, nil
}

func (f *fakeDocker) VolumeInspect(_ context.Context, name string) (runtime.Volume, error) {
	return runtime.Volume{Name: name, Driver: "local"}, nil
}

func (f *fakeDocker) StackSecretNames(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (f *fakeDocker) SecretList(_ context.Context, _, _ string) ([]string, error) {
	names := make([]string, 0, len(f.secrets))
	for n := range f.secrets {
		names = append(names, n)
	}
	return names, nil
}

// SecretInspect mimics the real Docker API: secret payloads are write-only,
// so inspect never returns Data (only labels survive). Callers that need to
// compare content must use the pmcluster.data_hash label.
func (f *fakeDocker) SecretInspect(_ context.Context, name string) (runtime.SecretInspectResult, error) {
	if s, ok := f.secrets[name]; ok {
		return runtime.SecretInspectResult{Labels: s.Labels}, nil
	}
	return runtime.SecretInspectResult{}, fmt.Errorf("secret %q not found", name)
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

func (f *fakeDocker) ConfigInspect(_ context.Context, name string) (runtime.ConfigInspectResult, error) {
	if c, ok := f.configs[name]; ok {
		return runtime.ConfigInspectResult{Labels: c.Labels, Data: c.Data}, nil
	}
	return runtime.ConfigInspectResult{}, fmt.Errorf("config %q not found", name)
}

func (f *fakeDocker) NodeList(_ context.Context) ([]runtime.Node, error) { return f.nodes, nil }

func (f *fakeDocker) SetNodeLabel(_ context.Context, _, key, value string) error {
	f.nodeLabels = append(f.nodeLabels, key+"="+value)
	return nil
}
func (f *fakeDocker) JoinTokens(_ context.Context) (runtime.JoinTokens, error) {
	return runtime.JoinTokens{}, nil
}

// SwarmID reports the Swarm cluster ID (the Raft cluster identity). "" when
// unset — tests mutate f.swarmID to exercise the swarm-identity detection.
func (f *fakeDocker) SwarmID(_ context.Context) (string, error) {
	return f.swarmID, nil
}

// Events is unused by the cluster package; return already-closed channels so
// this fake keeps satisfying the interface as it grows.
func (f *fakeDocker) Events(_ context.Context, _ time.Time) (<-chan runtime.Event, <-chan error) {
	evCh := make(chan runtime.Event)
	close(evCh)
	errCh := make(chan error)
	close(errCh)
	return evCh, errCh
}

func (f *fakeDocker) Close() error { return nil }

// Compile-time assertion.
var _ runtime.Client = (*fakeDocker)(nil)

// recordingDeployer captures deploy/remove calls for order + count assertions.
type recordingDeployer struct {
	deployedStacks []deployRecord
	removedStacks  []string
	forceUpdated   []string
	prunedStacks   []string
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

func (r *recordingDeployer) DeployStackNoPrune(_ context.Context, name string, composeYAML []byte) error {
	r.deployedStacks = append(r.deployedStacks, deployRecord{Name: name, YAMLLen: len(composeYAML), YAML: string(composeYAML)})
	return nil
}

func (r *recordingDeployer) PruneStack(_ context.Context, name string, _ []byte) error {
	r.prunedStacks = append(r.prunedStacks, name)
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
