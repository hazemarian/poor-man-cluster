package cluster

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestEdgeStackPublishesTheCredentialListenerOnTheMesh pins the publish shape of
// the credential listener in the RENDERED stack, not in the template text.
//
// It has to be `mode: ingress`. A node's docker-credential-pmcluster (a host
// process, invoked by dockerd) dials 127.0.0.1:9443 — a constant. In host mode
// that port exists only on the node running the edge task, so every other
// node's helper would dial a closed port; the routing mesh is what makes one
// address correct everywhere. Same reasoning, and the same failure mode, as the
// collector's 4318 (TestObservabilityOTLPIsPublishedOnTheMesh).
func TestEdgeStackPublishesTheCredentialListenerOnTheMesh(t *testing.T) {
	body, err := LoadComposeFile(StackEdge, RenderInput{
		Domain:            "example.com",
		LeaderNode:        "nxt-sw-1-m",
		EdgeLoginDisabled: true,
	})
	if err != nil {
		t.Fatalf("render edge stack: %v", err)
	}

	var doc struct {
		Services map[string]struct {
			Ports       []stackPort       `json:"ports"`
			Environment map[string]string `json:"environment"`
			Volumes     []string          `json:"volumes"`
		} `json:"services"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("rendered edge stack is not valid YAML: %v", err)
	}
	svc, ok := doc.Services["pmcluster-edge"]
	if !ok {
		t.Fatalf("rendered stack has no pmcluster-edge service:\n%s", body)
	}

	if len(svc.Ports) != 1 {
		t.Fatalf("pmcluster-edge publishes %d ports, want exactly 1 (the credential listener): %+v", len(svc.Ports), svc.Ports)
	}
	p := svc.Ports[0]
	if p.Target != 9443 || p.Published != 9443 {
		t.Errorf("published port = %d->%d, want 9443->9443", p.Published, p.Target)
	}
	if p.Mode != "ingress" {
		t.Errorf("credential publish mode = %q, want \"ingress\": host mode binds 9443 only where the edge task runs, and every other node's helper dials 127.0.0.1", p.Mode)
	}
	if p.Protocol != "tcp" {
		t.Errorf("credential publish protocol = %q, want tcp", p.Protocol)
	}

	// The listener cannot authenticate a node without its swarm ID and the CA.
	if got := svc.Environment["PMCLUSTER_CRED_LISTEN"]; got != ":9443" {
		t.Errorf("PMCLUSTER_CRED_LISTEN = %q, want :9443", got)
	}

	caPath := svc.Environment["PMCLUSTER_CRED_CLIENT_CA"]
	if caPath == "" {
		t.Fatal("PMCLUSTER_CRED_CLIENT_CA is empty: the listener would have no client CA")
	}
	source := "/var/lib/docker/swarm/certificates/swarm-root-ca.crt"
	found := false
	for _, v := range svc.Volumes {
		if strings.HasPrefix(v, source+":") && strings.HasSuffix(v, ":ro") && strings.Contains(v, caPath) {
			found = true
		}
	}
	if !found {
		t.Errorf("the swarm root CA is not bind-mounted read-only at %s (volumes: %v)", caPath, svc.Volumes)
	}
}

// TestEdgeStackDoesNotRenderTheSwarmID: the listener resolves the swarm ID from
// the daemon at startup. Rendering it into the compose would make a swarm
// re-init change the rendered bytes — a forced redeploy of the console for a
// value the daemon already knows.
func TestEdgeStackDoesNotRenderTheSwarmID(t *testing.T) {
	body, err := LoadComposeFile(StackEdge, RenderInput{
		Domain:            "example.com",
		LeaderNode:        "nxt-sw-1-m",
		EdgeLoginDisabled: true,
	})
	if err != nil {
		t.Fatalf("render edge stack: %v", err)
	}
	if strings.Contains(string(body), "PMCLUSTER_SWARM_ID") {
		t.Error("the rendered edge stack carries PMCLUSTER_SWARM_ID; the listener reads it from the daemon")
	}
}
