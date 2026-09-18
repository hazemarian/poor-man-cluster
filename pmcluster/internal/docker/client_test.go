package docker

import (
	"bytes"
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

	// services back the service-ops surface (ServiceInspect/Tasks/Logs/
	// Restart/Exec). Nil maps are lazily initialised on first write.
	services       map[string]ServiceInspectResult
	serviceTasks   map[string][]ServiceTask
	serviceLogs    map[string][]LogLine
	serviceRestart int           // count of ServiceRestart calls
	execResults    []*ExecResult // queue of exec results, consumed in order
	execErr        error
}

// AddService registers a swarm service for the service-ops tests.
func (f *fakeClient) AddService(name string, res ServiceInspectResult) {
	if f.services == nil {
		f.services = make(map[string]ServiceInspectResult)
	}
	res.Name = name
	f.services[name] = res
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

func (f *fakeClient) NodeList(_ context.Context) ([]Node, error) { return nil, nil }

func (f *fakeClient) ServiceList(_ context.Context) ([]Service, error) {
	out := make([]Service, 0, len(f.services))
	for _, s := range f.services {
		out = append(out, Service{
			ID:    s.ID,
			Name:  s.Name,
			Stack: s.Labels[StackNamespaceLabel],
			Image: s.Image,
		})
	}
	return out, nil
}

func (f *fakeClient) ServiceInspect(_ context.Context, name string) (ServiceInspectResult, error) {
	if s, ok := f.services[name]; ok {
		return s, nil
	}
	// Also resolve by service ID if it matches a registered service's ID.
	for _, s := range f.services {
		if s.ID == name {
			return s, nil
		}
	}
	return ServiceInspectResult{}, fmt.Errorf("service %q not found", name)
}

func (f *fakeClient) ServiceTasks(_ context.Context, serviceID string) ([]ServiceTask, error) {
	svc, err := f.ServiceInspect(context.Background(), serviceID)
	if err != nil {
		return nil, err
	}
	out := make([]ServiceTask, len(f.serviceTasks[svc.Name]))
	copy(out, f.serviceTasks[svc.Name])
	return out, nil
}

func (f *fakeClient) ServiceLogs(_ context.Context, serviceID string, _ int) ([]LogLine, error) {
	svc, err := f.ServiceInspect(context.Background(), serviceID)
	if err != nil {
		return nil, err
	}
	out := make([]LogLine, len(f.serviceLogs[svc.Name]))
	copy(out, f.serviceLogs[svc.Name])
	return out, nil
}

func (f *fakeClient) ServiceRestart(_ context.Context, serviceID string) error {
	if _, err := f.ServiceInspect(context.Background(), serviceID); err != nil {
		return err
	}
	f.serviceRestart++
	return nil
}

func (f *fakeClient) ServiceExec(_ context.Context, serviceID string, _ []string) (*ExecResult, error) {
	if f.execErr != nil {
		return nil, f.execErr
	}
	if len(f.execResults) == 0 {
		return nil, fmt.Errorf("service %q: no running task on this node", serviceID)
	}
	res := f.execResults[0]
	f.execResults = f.execResults[1:]
	return res, nil
}
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

// TestSplitLines verifies the log-line splitter drops empty lines and tags
// the stream correctly.
func TestSplitLines(t *testing.T) {
	got := splitLines("stdout", "a\nb\n\n")
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2: %+v", len(got), got)
	}
	if got[0] != (LogLine{Stream: "stdout", Line: "a"}) {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1] != (LogLine{Stream: "stdout", Line: "b"}) {
		t.Errorf("got[1] = %+v", got[1])
	}
}

// TestDemuxLines verifies the multiplexed-stream parser preserves stream tags
// and interleaved order, and caps the tail.
func TestDemuxLines(t *testing.T) {
	// Frame format: [stream(1) pad(3) size(4 big-endian)][payload].
	frame := func(stream byte, payload string) []byte {
		b := make([]byte, 8+len(payload))
		b[0] = stream
		b[4] = byte(len(payload) >> 24)
		b[5] = byte(len(payload) >> 16)
		b[6] = byte(len(payload) >> 8)
		b[7] = byte(len(payload))
		copy(b[8:], payload)
		return b
	}
	var stream []byte
	stream = append(stream, frame(1, "out1\n")...)
	stream = append(stream, frame(2, "err1\n")...)
	stream = append(stream, frame(1, "out2\n")...)

	lines, err := demuxLines(bytes.NewReader(stream), 100)
	if err != nil {
		t.Fatalf("demuxLines: %v", err)
	}
	want := []LogLine{
		{Stream: "stdout", Line: "out1"},
		{Stream: "stderr", Line: "err1"},
		{Stream: "stdout", Line: "out2"},
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d: %+v", len(lines), len(want), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("lines[%d] = %+v, want %+v", i, lines[i], want[i])
		}
	}

	// Tail cap: only the last 2 lines survive.
	lines, err = demuxLines(bytes.NewReader(stream), 2)
	if err != nil {
		t.Fatalf("demuxLines tail: %v", err)
	}
	if len(lines) != 2 || lines[0] != (LogLine{Stream: "stderr", Line: "err1"}) {
		t.Errorf("tail lines = %+v, want [err1 out2]", lines)
	}
}

// TestFakeClient_ServiceOps verifies the fake's service-ops surface.
func TestFakeClient_ServiceOps(t *testing.T) {
	f := &fakeClient{}
	f.AddService("demo_web", ServiceInspectResult{
		ID:     "svc-1",
		Image:  "ghcr.io/nextrum-sy/demo:latest",
		Labels: map[string]string{StackNamespaceLabel: "demo"},
	})
	f.serviceTasks = map[string][]ServiceTask{
		"demo_web": {{TaskID: "t1", State: "running"}},
	}
	f.serviceLogs = map[string][]LogLine{
		"demo_web": {{Stream: "stdout", Line: "hello"}},
	}
	f.execResults = []*ExecResult{{ExitCode: 0, Stdout: "ok"}}

	if svc, err := f.ServiceInspect(context.Background(), "demo_web"); err != nil || svc.Image == "" {
		t.Errorf("ServiceInspect = %+v, %v", svc, err)
	}
	if tasks, err := f.ServiceTasks(context.Background(), "svc-1"); err != nil || len(tasks) != 1 {
		t.Errorf("ServiceTasks = %+v, %v", tasks, err)
	}
	if logs, err := f.ServiceLogs(context.Background(), "demo_web", 10); err != nil || len(logs) != 1 {
		t.Errorf("ServiceLogs = %+v, %v", logs, err)
	}
	if err := f.ServiceRestart(context.Background(), "demo_web"); err != nil || f.serviceRestart != 1 {
		t.Errorf("ServiceRestart err=%v calls=%d", err, f.serviceRestart)
	}
	res, err := f.ServiceExec(context.Background(), "demo_web", []string{"whoami"})
	if err != nil || res == nil || res.Stdout != "ok" {
		t.Errorf("ServiceExec = %+v, %v", res, err)
	}
	if list, err := f.ServiceList(context.Background()); err != nil || len(list) != 1 || list[0].Stack != "demo" {
		t.Errorf("ServiceList = %+v, %v", list, err)
	}
}
