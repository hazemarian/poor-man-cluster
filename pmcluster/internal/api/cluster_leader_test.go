package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/testutil/fakeclient"
)

// leaderMoveClient answers NodeList and records SetNodeRole, electing the next
// ready manager the way the swarm would.
type leaderMoveClient struct {
	*fakeclient.Client
	roleCalls []string
	electedBy string // hostname elected when the leader is demoted
}

func (c *leaderMoveClient) SetNodeRole(_ context.Context, id, role string) error {
	c.roleCalls = append(c.roleCalls, id+":"+role)
	nodes := c.NodeListResult
	for i := range nodes {
		if nodes[i].ID != id {
			continue
		}
		nodes[i].Role = role
		if role != "manager" {
			nodes[i].IsLeader = false
		}
	}
	if role == "worker" {
		for i := range nodes {
			if nodes[i].Role == "manager" && nodes[i].Hostname == c.electedBy {
				nodes[i].IsLeader = true
			}
		}
	}
	c.NodeListResult = nodes
	return nil
}

func leaderMoveFixture() *leaderMoveClient {
	return &leaderMoveClient{
		Client: &fakeclient.Client{NodeListResult: []runtime.Node{
			{Hostname: "nxt-sw-4-m", ID: "n4", Role: "manager", Status: "ready", Availability: "active", IsLeader: true},
			{Hostname: "nxt-sw-5-m", ID: "n5", Role: "manager", Status: "ready", Availability: "active"},
			{Hostname: "nxt-sw-6-m", ID: "n6", Role: "manager", Status: "ready", Availability: "active"},
			{Hostname: "nxt-sw-7-m", ID: "n7", Role: "manager", Status: "ready", Availability: "active"},
		}},
		electedBy: "nxt-sw-5-m",
	}
}

// The route exists so the CLI can move leadership in remote mode; the response
// reports where leadership actually landed.
func TestClusterLeaderHandlerReportsTheNewLeader(t *testing.T) {
	prev := leaderMoveOptions
	leaderMoveOptions = cluster.LeaderMoveOptions{PollInterval: time.Millisecond, SettleTimeout: 200 * time.Millisecond, ElectionGrace: time.Millisecond, MaxElections: 3, Budget: 2 * time.Second}
	t.Cleanup(func() { leaderMoveOptions = prev })

	c := leaderMoveFixture()
	r := chi.NewRouter()
	r.Post("/cluster/leader/{hostname}", ClusterLeaderHandler(c))

	req := httptest.NewRequest(http.MethodPost, "/cluster/leader/nxt-sw-5-m", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Leader    string `json:"leader"`
		Previous  string `json:"previous"`
		Elections int    `json:"elections"`
		Changed   bool   `json:"changed"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Leader != "nxt-sw-5-m" || body.Previous != "nxt-sw-4-m" || body.Elections != 1 || !body.Changed {
		t.Fatalf("body = %+v, want the new leader and one election", body)
	}
	if len(c.roleCalls) != 2 {
		t.Fatalf("role calls = %v, want the old leader demoted and restored", c.roleCalls)
	}
}

// A refusal is an answer the operator can act on, so it is a 409 with the
// reason in it — not a 500.
func TestClusterLeaderHandlerRefusesAWorkerWithConflict(t *testing.T) {
	c := leaderMoveFixture()
	c.NodeListResult = append(c.NodeListResult, runtime.Node{
		Hostname: "nxt-sw-3-w", ID: "n3", Role: "worker", Status: "ready", Availability: "active",
	})
	r := chi.NewRouter()
	r.Post("/cluster/leader/{hostname}", ClusterLeaderHandler(c))

	req := httptest.NewRequest(http.MethodPost, "/cluster/leader/nxt-sw-3-w", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "only a manager can win") {
		t.Fatalf("the refusal must say why: %s", rec.Body.String())
	}
	if len(c.roleCalls) != 0 {
		t.Fatalf("a refusal must not touch any role, calls = %v", c.roleCalls)
	}
}
