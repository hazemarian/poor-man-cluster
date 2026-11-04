package api

import "testing"

func TestIsLoopbackRefusesEverythingElse(t *testing.T) {
	local := []string{"127.0.0.1:54321", "127.0.0.2:1", "[::1]:8080"}
	for _, a := range local {
		if !isLoopback(a) {
			t.Fatalf("%q should be local", a)
		}
	}
	// The edge proxy container reaches the daemon over the bridge; the cluster
	// plane is reachable from other nodes. Neither may read the decision.
	remote := []string{"10.7.224.5:40001", "172.18.0.3:9090", "217.160.143.26:5000", "", "not-an-addr", "localhost:9090"}
	for _, a := range remote {
		if isLoopback(a) {
			t.Fatalf("%q must not be treated as local", a)
		}
	}
}
