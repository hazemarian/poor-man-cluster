package api

import "testing"

func TestIsClusterReachableAllowsTheClusterPlaneOnly(t *testing.T) {
	// A worker's agent asks a manager over the swarm plane, so private addresses
	// must be accepted — a worker's own daemon serves nothing at all.
	local := []string{"127.0.0.1:54321", "127.0.0.2:1", "[::1]:8080", "10.7.224.5:40001", "172.18.0.3:9090", "192.168.1.9:1"}
	for _, a := range local {
		if !isClusterReachable(a) {
			t.Fatalf("%q should be reachable on the cluster plane", a)
		}
	}
	// Public addresses must not: the decision is for the platform's own nodes.
	public := []string{"217.160.143.26:5000", "8.8.8.8:53", "", "not-an-addr", "localhost:9090"}
	for _, a := range public {
		if isClusterReachable(a) {
			t.Fatalf("%q must not be treated as a cluster caller", a)
		}
	}
}
