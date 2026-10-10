package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/testutil/fakeclient"
)

// TestClusterInfoHandler_HappyPath verifies 200 and a well-shaped JSON body.
func TestClusterInfoHandler_HappyPath(t *testing.T) {
	fake := &fakeclient.Client{
		InfoResult: runtime.Info{
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
	fake := &fakeclient.Client{
		InfoErr: errors.New("cannot connect to docker daemon"),
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
