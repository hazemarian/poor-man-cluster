package docker

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// fakeClient is an in-process implementation of the Client interface that lets
// callers (handlers, etc.) be tested without touching /var/run/docker.sock.
//
// It is defined here in the docker package so it lives alongside the real
// client and can be imported by other packages' tests.
type fakeClient struct {
	pingResult Ping
	pingErr    error
	infoResult Info
	infoErr    error
	closed     bool

	// In-memory stores for the cluster bootstrap surface (Phase 2).
	// Nil maps are lazily initialised on first write so simple
	// instantiation (`&fakeClient{}`) keeps working.
	networks map[string]NetworkSpec
	secrets  map[string]SecretSpec
	configs  map[string]ConfigSpec

	// volumes + stackSecrets back VolumeList / StackSecretNames.
	volumes      []string
	stackSecrets []string
}

func (f *fakeClient) Ping(_ context.Context) (Ping, error) {
	return f.pingResult, f.pingErr
}

func (f *fakeClient) Info(_ context.Context) (Info, error) {
	return f.infoResult, f.infoErr
}

func (f *fakeClient) NetworkExists(_ context.Context, name string) (bool, error) {
	_, ok := f.networks[name]
	return ok, nil
}

func (f *fakeClient) NetworkCreate(_ context.Context, spec NetworkSpec) error {
	if f.networks == nil {
		f.networks = make(map[string]NetworkSpec)
	}
	f.networks[spec.Name] = spec
	return nil
}

func (f *fakeClient) SecretExists(_ context.Context, name string) (bool, error) {
	_, ok := f.secrets[name]
	return ok, nil
}

func (f *fakeClient) SecretCreate(_ context.Context, spec SecretSpec) error {
	if f.secrets == nil {
		f.secrets = make(map[string]SecretSpec)
	}
	f.secrets[spec.Name] = spec
	return nil
}

func (f *fakeClient) ConfigExists(_ context.Context, name string) (bool, error) {
	_, ok := f.configs[name]
	return ok, nil
}

func (f *fakeClient) ConfigCreate(_ context.Context, spec ConfigSpec) error {
	if f.configs == nil {
		f.configs = make(map[string]ConfigSpec)
	}
	f.configs[spec.Name] = spec
	return nil
}

func (f *fakeClient) SecretRemove(_ context.Context, name string) error {
	delete(f.secrets, name)
	return nil
}

func (f *fakeClient) ConfigRemove(_ context.Context, name string) error {
	delete(f.configs, name)
	return nil
}

func (f *fakeClient) NetworkRemove(_ context.Context, name string) error {
	delete(f.networks, name)
	return nil
}

func (f *fakeClient) VolumeRemove(_ context.Context, name string) error {
	return nil
}

func (f *fakeClient) NodeList(_ context.Context) ([]Node, error)       { return nil, nil }
func (f *fakeClient) ServiceList(_ context.Context) ([]Service, error) { return nil, nil }
func (f *fakeClient) JoinTokens(_ context.Context) (JoinTokens, error) { return JoinTokens{}, nil }
func (f *fakeClient) SecretList(_ context.Context, _, _ string) ([]string, error) {
	names := make([]string, 0, len(f.secrets))
	for n := range f.secrets {
		names = append(names, n)
	}
	return names, nil
}

func (f *fakeClient) VolumeList(_ context.Context, _, _ string) ([]string, error) {
	out := make([]string, len(f.volumes))
	copy(out, f.volumes)
	return out, nil
}

func (f *fakeClient) StackSecretNames(_ context.Context, _ string) ([]string, error) {
	out := make([]string, len(f.stackSecrets))
	copy(out, f.stackSecrets)
	return out, nil
}

// SecretInspect mimics the real Docker API: secret payloads are write-only,
// so inspect never returns Data (only labels survive). Callers that need to
// compare content must use the pmcluster.data_hash label.
func (f *fakeClient) SecretInspect(_ context.Context, name string) (SecretInspectResult, error) {
	if s, ok := f.secrets[name]; ok {
		return SecretInspectResult{Labels: s.Labels}, nil
	}
	return SecretInspectResult{}, fmt.Errorf("secret %q not found", name)
}
func (f *fakeClient) ConfigList(_ context.Context, _, _ string) ([]string, error) {
	names := make([]string, 0, len(f.configs))
	for n := range f.configs {
		names = append(names, n)
	}
	return names, nil
}

func (f *fakeClient) ConfigInspect(_ context.Context, name string) (ConfigInspectResult, error) {
	if c, ok := f.configs[name]; ok {
		return ConfigInspectResult{Labels: c.Labels, Data: c.Data}, nil
	}
	return ConfigInspectResult{}, fmt.Errorf("config %q not found", name)
}

func (f *fakeClient) Close() error {
	f.closed = true
	return nil
}

// Compile-time assertion: fakeClient satisfies the Client interface.
var _ Client = (*fakeClient)(nil)

// TestFakeClient_InterfaceConsumable verifies that fakeClient satisfies Client
// and that the Ping / Info / Close methods route calls and return values correctly.
func TestFakeClient_InterfaceConsumable(t *testing.T) {
	f := &fakeClient{
		pingResult: Ping{APIVersion: "1.45", OSType: "linux", Experimental: false},
		infoResult: Info{
			Name:                  "test-node",
			ServerVersion:         "26.0.0",
			OperatingSystem:       "Alpine Linux",
			Architecture:          "x86_64",
			NCPU:                  4,
			MemTotal:              8 * 1024 * 1024 * 1024,
			SwarmLocalNodeState:   "active",
			SwarmControlAvailable: true,
			SwarmManagers:         1,
			SwarmNodes:            3,
		},
	}

	p, err := f.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if p.APIVersion != "1.45" {
		t.Errorf("Ping.APIVersion = %q, want 1.45", p.APIVersion)
	}
	if p.OSType != "linux" {
		t.Errorf("Ping.OSType = %q, want linux", p.OSType)
	}

	info, err := f.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != "test-node" {
		t.Errorf("Info.Name = %q, want test-node", info.Name)
	}
	if info.NCPU != 4 {
		t.Errorf("Info.NCPU = %d, want 4", info.NCPU)
	}
	if info.SwarmLocalNodeState != "active" {
		t.Errorf("Info.SwarmLocalNodeState = %q, want active", info.SwarmLocalNodeState)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !f.closed {
		t.Error("Close did not set closed=true")
	}
}

// TestFakeClient_PingError verifies that ping error propagation works correctly.
func TestFakeClient_PingError(t *testing.T) {
	f := &fakeClient{pingErr: errors.New("connection refused")}
	_, err := f.Ping(context.Background())
	if err == nil {
		t.Fatal("expected error from Ping, got nil")
	}
}

// TestFakeClient_InfoError verifies that info error propagation works correctly.
func TestFakeClient_InfoError(t *testing.T) {
	f := &fakeClient{infoErr: errors.New("dial unix /var/run/docker.sock: no such file")}
	_, err := f.Info(context.Background())
	if err == nil {
		t.Fatal("expected error from Info, got nil")
	}
}

// TestFakeClient_TableDriven exercises multiple configurations to demonstrate
// the interface is fully mockable for handler tests in other packages.
func TestFakeClient_TableDriven(t *testing.T) {
	cases := []struct {
		name       string
		info       Info
		infoErr    error
		wantErrNil bool
	}{
		{
			name: "happy path",
			info: Info{Name: "node-a", SwarmLocalNodeState: "active"},
		},
		{
			name:    "docker unreachable",
			infoErr: errors.New("docker: no such socket"),
		},
		{
			name: "swarm inactive",
			info: Info{Name: "node-b", SwarmLocalNodeState: "inactive"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeClient{infoResult: tc.info, infoErr: tc.infoErr}
			info, err := f.Info(context.Background())
			if tc.infoErr != nil {
				if err == nil {
					t.Errorf("expected error, got nil info=%+v", info)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if info.Name != tc.info.Name {
					t.Errorf("info.Name = %q, want %q", info.Name, tc.info.Name)
				}
			}
		})
	}
}
