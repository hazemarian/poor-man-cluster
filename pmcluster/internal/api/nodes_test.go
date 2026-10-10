package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/testutil/fakeclient"
)

// storeFake implements the setting-store interface NodesHandler needs.
type storeFake struct {
	settings map[string]string
}

func newStoreFake(initial map[string]string) *storeFake {
	s := &storeFake{settings: map[string]string{}}
	for k, v := range initial {
		s.settings[k] = v
	}
	return s
}

func (s *storeFake) SettingDefault(_ context.Context, key, def string) string {
	if v, ok := s.settings[key]; ok {
		return v
	}
	return def
}

func (s *storeFake) SetSetting(_ context.Context, key, value string) error {
	s.settings[key] = value
	return nil
}

func TestNodesHandler_HappyPath(t *testing.T) {
	now := int64(1_700_000_000)
	fake := &fakeclient.Client{
		NodeListResult: []runtime.Node{
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

	h := NodesHandler(fake, newStoreFake(nil))
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
	fake := &fakeclient.Client{
		NodeListErr: errors.New("cannot connect to docker daemon"),
	}

	h := NodesHandler(fake, newStoreFake(nil))
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
	fake := &fakeclient.Client{
		NodeListResult: []runtime.Node{},
	}

	h := NodesHandler(fake, newStoreFake(nil))
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

// storageLabelClient wraps fakeclient.Client and records SetNodeLabel calls so
// NodeStorageHandler tests can assert the label was stamped/cleared and inject
// failures.
type storageLabelClient struct {
	*fakeclient.Client
	labelCalls []string // "id:key:value"
	labelErr   error
}

func (c *storageLabelClient) SetNodeLabel(_ context.Context, nodeID, key, value string) error {
	c.labelCalls = append(c.labelCalls, nodeID+":"+key+":"+value)
	return c.labelErr
}

func tc7Nodes() []runtime.Node {
	now := int64(1_700_000_000)
	return []runtime.Node{
		{ID: "node1abc", Hostname: "nxt-sw-1-m", Role: "manager", Availability: "active", Status: "ready", IsLeader: true, EngineVersion: "27.0.0", Address: "10.0.0.1:2377", CreatedAt: now, UpdatedAt: now + 60},
		{ID: "node2xyz", Hostname: "nxt-sw-2-m", Role: "worker", Availability: "active", Status: "ready", IsLeader: false, EngineVersion: "27.0.0", CreatedAt: now + 100, UpdatedAt: now + 200},
	}
}

// doStorageRequest routes a promote/demote request through a chi router so
// chi.URLParam("hostname") is populated.
func doStorageRequest(t *testing.T, h http.Handler, method, hostname string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/nodes/{hostname}/storage", h.ServeHTTP)
	r.Delete("/nodes/{hostname}/storage", h.ServeHTTP)
	req := httptest.NewRequest(method, "/nodes/"+hostname+"/storage", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestNodeStorageHandler_Promote(t *testing.T) {
	cli := &storageLabelClient{Client: &fakeclient.Client{NodeListResult: tc7Nodes()}}
	store := newStoreFake(map[string]string{cluster.SettingStorageNodes(): "nxt-sw-1-m"})

	h := NodeStorageHandler(cli, store)
	rec := doStorageRequest(t, h, http.MethodPost, "nxt-sw-2-m")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["storage"] != true {
		t.Errorf("storage = %v, want true", body["storage"])
	}
	if body["storage_nodes"] != "nxt-sw-1-m,nxt-sw-2-m" {
		t.Errorf("storage_nodes = %v, want nxt-sw-1-m,nxt-sw-2-m", body["storage_nodes"])
	}
	if store.SettingDefault(context.Background(), cluster.SettingStorageNodes(), "") != "nxt-sw-1-m,nxt-sw-2-m" {
		t.Errorf("setting not persisted: %q", store.SettingDefault(context.Background(), cluster.SettingStorageNodes(), ""))
	}
	if len(cli.labelCalls) != 1 || cli.labelCalls[0] != "node2xyz:"+runtime.StorageNodeLabel+":true" {
		t.Errorf("labelCalls = %v, want one true stamp on node2xyz", cli.labelCalls)
	}
}

func TestNodeStorageHandler_PromoteIdempotent(t *testing.T) {
	cli := &storageLabelClient{Client: &fakeclient.Client{NodeListResult: tc7Nodes()}}
	store := newStoreFake(map[string]string{cluster.SettingStorageNodes(): "nxt-sw-1-m,nxt-sw-2-m"})

	h := NodeStorageHandler(cli, store)
	rec := doStorageRequest(t, h, http.MethodPost, "nxt-sw-2-m")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["storage_nodes"] != "nxt-sw-1-m,nxt-sw-2-m" {
		t.Errorf("storage_nodes = %v, want unchanged", body["storage_nodes"])
	}
	// Still re-stamps the label even when the list didn't change.
	if len(cli.labelCalls) != 1 {
		t.Errorf("labelCalls = %v, want exactly one", cli.labelCalls)
	}
}

func TestNodeStorageHandler_Demote(t *testing.T) {
	cli := &storageLabelClient{Client: &fakeclient.Client{NodeListResult: tc7Nodes()}}
	store := newStoreFake(map[string]string{cluster.SettingStorageNodes(): "nxt-sw-1-m,nxt-sw-2-m"})

	h := NodeStorageHandler(cli, store)
	rec := doStorageRequest(t, h, http.MethodDelete, "nxt-sw-2-m")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["storage"] != false {
		t.Errorf("storage = %v, want false", body["storage"])
	}
	if body["storage_nodes"] != "nxt-sw-1-m" {
		t.Errorf("storage_nodes = %v, want nxt-sw-1-m", body["storage_nodes"])
	}
	if len(cli.labelCalls) != 1 || cli.labelCalls[0] != "node2xyz:"+runtime.StorageNodeLabel+":" {
		t.Errorf("labelCalls = %v, want one empty-value clear on node2xyz", cli.labelCalls)
	}
}

func TestNodeStorageHandler_UnknownNode_404(t *testing.T) {
	cli := &storageLabelClient{Client: &fakeclient.Client{NodeListResult: tc7Nodes()}}
	store := newStoreFake(nil)

	h := NodeStorageHandler(cli, store)
	rec := doStorageRequest(t, h, http.MethodPost, "ghost-node")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if len(cli.labelCalls) != 0 {
		t.Errorf("labelCalls = %v, want none for unknown node", cli.labelCalls)
	}
	if store.SettingDefault(context.Background(), cluster.SettingStorageNodes(), "") != "" {
		t.Errorf("setting changed for unknown node: %q", store.SettingDefault(context.Background(), cluster.SettingStorageNodes(), ""))
	}
}

func TestNodeStorageHandler_LabelError_500(t *testing.T) {
	cli := &storageLabelClient{Client: &fakeclient.Client{NodeListResult: tc7Nodes()}, labelErr: errors.New("docker label fail")}
	store := newStoreFake(nil)

	h := NodeStorageHandler(cli, store)
	rec := doStorageRequest(t, h, http.MethodPost, "nxt-sw-2-m")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	// Setting was still updated before the label failure.
	if store.SettingDefault(context.Background(), cluster.SettingStorageNodes(), "") != "nxt-sw-2-m" {
		t.Errorf("storage_nodes = %q, want nxt-sw-2-m (setting survives label error)", store.SettingDefault(context.Background(), cluster.SettingStorageNodes(), ""))
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg, _ := body["error"].(string); msg == "" || !strings.Contains(msg, "cluster update") {
		t.Errorf("error body = %v, want repair hint", body["error"])
	}
}

func TestNodesHandler_StorageFlag(t *testing.T) {
	fake := &fakeclient.Client{NodeListResult: tc7Nodes()}
	h := NodesHandler(fake, newStoreFake(map[string]string{cluster.SettingStorageNodes(): "nxt-sw-1-m"}))

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
	nodes, _ := body["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("len(nodes) = %d, want 2", len(nodes))
	}
	m0 := nodes[0].(map[string]any)
	m1 := nodes[1].(map[string]any)
	if m0["storage"] != true {
		t.Errorf("nodes[0].storage = %v, want true (in storage_nodes)", m0["storage"])
	}
	if m1["storage"] != false {
		t.Errorf("nodes[1].storage = %v, want false", m1["storage"])
	}
}
