package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

func TestNodesHandler_HappyPath(t *testing.T) {
	now := int64(1_700_000_000)
	fake := &inMemoryDockerClient{
		nodeListResult: []runtime.Node{
			{
				ID:            "node1abc",
				Hostname:      "manager-01",
				Role:          "manager",
				Availability:  "active",
				Status:        "ready",
				IsLeader:      true,
				EngineVersion: "27.0.0",
				Address:       "10.0.0.1:2377",
				CreatedAt:     now,
				UpdatedAt:     now + 60,
			},
			{
				ID:            "node2xyz",
				Hostname:      "worker-01",
				Role:          "worker",
				Availability:  "active",
				Status:        "ready",
				IsLeader:      false,
				EngineVersion: "27.0.0",
				Address:       "",
				CreatedAt:     now + 100,
				UpdatedAt:     now + 200,
			},
		},
	}

	h := NodesHandler(fake)
	req := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	nodes, ok := body["nodes"].([]any)
	if !ok {
		t.Fatalf("'nodes' field missing or wrong type: %T", body["nodes"])
	}
	if len(nodes) != 2 {
		t.Fatalf("len(nodes) = %d, want 2", len(nodes))
	}

	checkNodeField := func(t *testing.T, idx int, key string, want any) {
		t.Helper()
		node, ok := nodes[idx].(map[string]any)
		if !ok {
			t.Fatalf("nodes[%d] is not a map: %T", idx, nodes[idx])
		}
		got := node[key]
		switch w := want.(type) {
		case string:
			if v, _ := got.(string); v != w {
				t.Errorf("nodes[%d].%s = %q, want %q", idx, key, v, w)
			}
		case bool:
			if v, _ := got.(bool); v != w {
				t.Errorf("nodes[%d].%s = %v, want %v", idx, key, v, w)
			}
		case float64:
			if v, _ := got.(float64); v != w {
				t.Errorf("nodes[%d].%s = %v, want %v", idx, key, v, w)
			}
		}
	}

	checkNodeField(t, 0, "id", "node1abc")
	checkNodeField(t, 0, "hostname", "manager-01")
	checkNodeField(t, 0, "role", "manager")
	checkNodeField(t, 0, "availability", "active")
	checkNodeField(t, 0, "status", "ready")
	checkNodeField(t, 0, "is_leader", true)
	checkNodeField(t, 0, "engine_version", "27.0.0")
	checkNodeField(t, 0, "address", "10.0.0.1:2377")
	checkNodeField(t, 0, "created_at", float64(now))
	checkNodeField(t, 0, "updated_at", float64(now+60))

	checkNodeField(t, 1, "id", "node2xyz")
	checkNodeField(t, 1, "hostname", "worker-01")
	checkNodeField(t, 1, "role", "worker")
	checkNodeField(t, 1, "is_leader", false)
	checkNodeField(t, 1, "address", "")
}

func TestNodesHandler_DockerError_502(t *testing.T) {
	fake := &inMemoryDockerClient{
		nodeListErr: errors.New("cannot connect to docker daemon"),
	}

	h := NodesHandler(fake)
	req := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
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

func TestNodesHandler_EmptyNodeList(t *testing.T) {
	fake := &inMemoryDockerClient{
		nodeListResult: []runtime.Node{},
	}

	h := NodesHandler(fake)
	req := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	nodes, ok := body["nodes"].([]any)
	if !ok {
		t.Fatalf("'nodes' field missing or wrong type: %T", body["nodes"])
	}
	if len(nodes) != 0 {
		t.Errorf("expected empty nodes array, got %d elements", len(nodes))
	}
}
