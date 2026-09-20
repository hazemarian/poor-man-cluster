package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// inMemoryDockerClient is a local fake that implements runtime.Client for use
// in the api package tests without importing the docker package's test file.
// (The canonical fake lives in docker/client_test.go; this copy lives here
// to keep api tests self-contained and avoid a test-only import cycle.)
type inMemoryDockerClient struct {
	infoResult     runtime.Info
	infoErr        error
	nodeListResult []runtime.Node
	nodeListErr    error
}

func (f *inMemoryDockerClient) Ping(_ context.Context) (runtime.Ping, error) {
	return runtime.Ping{}, nil
}
func (f *inMemoryDockerClient) Info(_ context.Context) (runtime.Info, error) {
	return f.infoResult, f.infoErr
}

// Network/Secret methods are unused by /api/cluster/info; stub them so this
// fake satisfies the runtime.Client interface as it grows in later phases.
func (f *inMemoryDockerClient) NetworkExists(_ context.Context, _ string) (bool, error) {
	return false, nil
}
func (f *inMemoryDockerClient) NetworkCreate(_ context.Context, _ runtime.NetworkSpec) error {
	return nil
}
func (f *inMemoryDockerClient) SecretExists(_ context.Context, _ string) (bool, error) {
	return false, nil
}
func (f *inMemoryDockerClient) SecretCreate(_ context.Context, _ runtime.SecretSpec) error {
	return nil
}
func (f *inMemoryDockerClient) ConfigExists(_ context.Context, _ string) (bool, error) {
	return false, nil
}
func (f *inMemoryDockerClient) ConfigCreate(_ context.Context, _ runtime.ConfigSpec) error {
	return nil
}
func (f *inMemoryDockerClient) SecretRemove(_ context.Context, _ string) error { return nil }
func (f *inMemoryDockerClient) SecretList(_ context.Context, _, _ string) ([]string, error) {
	return nil, nil
}
func (f *inMemoryDockerClient) SecretInspect(_ context.Context, _ string) (runtime.SecretInspectResult, error) {
	return runtime.SecretInspectResult{}, nil
}
func (f *inMemoryDockerClient) ConfigRemove(_ context.Context, _ string) error  { return nil }
func (f *inMemoryDockerClient) NetworkRemove(_ context.Context, _ string) error { return nil }
func (f *inMemoryDockerClient) VolumeRemove(_ context.Context, _ string) error  { return nil }
func (f *inMemoryDockerClient) VolumeList(_ context.Context, _, _ string) ([]string, error) {
	return nil, nil
}
func (f *inMemoryDockerClient) StackSecretNames(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (f *inMemoryDockerClient) ServiceList(_ context.Context) ([]runtime.Service, error) {
	return nil, nil
}
func (f *inMemoryDockerClient) ServiceInspect(_ context.Context, _ string) (runtime.ServiceInspectResult, error) {
	return runtime.ServiceInspectResult{}, nil
}
func (f *inMemoryDockerClient) ServiceTasks(_ context.Context, _ string) ([]runtime.ServiceTask, error) {
	return nil, nil
}
func (f *inMemoryDockerClient) ServiceLogs(_ context.Context, _ string, _ int) ([]runtime.LogLine, error) {
	return nil, nil
}
func (f *inMemoryDockerClient) ServiceRestart(_ context.Context, _ string) error { return nil }
func (f *inMemoryDockerClient) ServiceExec(_ context.Context, _ string, _ []string) (*runtime.ExecResult, error) {
	return nil, nil
}
func (f *inMemoryDockerClient) ConfigList(_ context.Context, _, _ string) ([]string, error) {
	return nil, nil
}
func (f *inMemoryDockerClient) ConfigInspect(_ context.Context, _ string) (runtime.ConfigInspectResult, error) {
	return runtime.ConfigInspectResult{}, nil
}
func (f *inMemoryDockerClient) NodeList(_ context.Context) ([]runtime.Node, error) {
	return f.nodeListResult, f.nodeListErr
}
func (f *inMemoryDockerClient) JoinTokens(_ context.Context) (runtime.JoinTokens, error) {
	return runtime.JoinTokens{}, nil
}
func (f *inMemoryDockerClient) Close() error { return nil }

var _ runtime.Client = (*inMemoryDockerClient)(nil)

// TestClusterInfoHandler_HappyPath verifies 200 and a well-shaped JSON body.
func TestClusterInfoHandler_HappyPath(t *testing.T) {
	fake := &inMemoryDockerClient{
		infoResult: runtime.Info{
			Name:                  "mgr-01",
			ServerVersion:         "27.0.0",
			OperatingSystem:       "Ubuntu 22.04",
			Architecture:          "aarch64",
			NCPU:                  8,
			MemTotal:              16 * 1024 * 1024 * 1024,
			SwarmLocalNodeState:   "active",
			SwarmControlAvailable: true,
			SwarmManagers:         3,
			SwarmNodes:            5,
		},
	}
	h := ClusterInfoHandler(fake)

	req := httptest.NewRequest(http.MethodGet, "/api/cluster/info", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	checkString := func(key, want string) {
		t.Helper()
		if v, _ := body[key].(string); v != want {
			t.Errorf("%s = %q, want %q", key, v, want)
		}
	}
	checkString("node_name", "mgr-01")
	checkString("server_version", "27.0.0")
	checkString("os", "Ubuntu 22.04")
	checkString("arch", "aarch64")

	if cpus, _ := body["cpus"].(float64); cpus != 8 {
		t.Errorf("cpus = %v, want 8", body["cpus"])
	}
	if mem, _ := body["memory_bytes"].(float64); mem != float64(16*1024*1024*1024) {
		t.Errorf("memory_bytes = %v, want %d", body["memory_bytes"], 16*1024*1024*1024)
	}

	swarm, ok := body["swarm"].(map[string]any)
	if !ok {
		t.Fatalf("swarm field missing or wrong type: %T", body["swarm"])
	}
	if swarm["state"] != "active" {
		t.Errorf("swarm.state = %v, want active", swarm["state"])
	}
	if swarm["control_available"] != true {
		t.Errorf("swarm.control_available = %v, want true", swarm["control_available"])
	}
	if swarm["managers"].(float64) != 3 {
		t.Errorf("swarm.managers = %v, want 3", swarm["managers"])
	}
	if swarm["nodes"].(float64) != 5 {
		t.Errorf("swarm.nodes = %v, want 5", swarm["nodes"])
	}
}

// TestClusterInfoHandler_BadGateway verifies 502 and an error field when the
// docker client returns an error.
func TestClusterInfoHandler_BadGateway(t *testing.T) {
	fake := &inMemoryDockerClient{
		infoErr: errors.New("cannot connect to docker daemon"),
	}
	h := ClusterInfoHandler(fake)

	req := httptest.NewRequest(http.MethodGet, "/api/cluster/info", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["error"]; !ok {
		t.Error("expected 'error' key in 502 response body")
	}
}
