package runners

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// NodePolicy is the part of the leader's decision that a NODE-level process needs:
// the watchdog, which runs as root outside the daemon and cannot read the store (and
// on a worker there is no store at all).
//
// It is written by whatever applied the decision — the manager loop on a manager, the
// runner agent on a worker — and read by `pmcluster health-watch`. Nothing else in the
// platform reads it, and a missing file means "not armed", which is the safe default.
type NodePolicy struct {
	// DockerRestart arms the watchdog to restart docker.service when its socket stops
	// answering. False unless an operator turned it on.
	DockerRestart bool `json:"docker_restart"`
}

// NodePolicyPath is where the policy document lives for a given data directory.
func NodePolicyPath(dir string) string { return filepath.Join(dir, "node-policy.json") }

// WriteNodePolicy writes the document atomically. Best effort by contract: a node
// that cannot write its policy keeps the previous one, which is safer than failing a
// reconcile over a watcher setting.
func WriteNodePolicy(dir string, p NodePolicy) error {
	body, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := NodePolicyPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, NodePolicyPath(dir))
}

// ReadNodePolicy reads the document, returning the zero value when it is missing or
// unreadable: "not armed" is the answer that cannot cause an unwanted restart.
func ReadNodePolicy(dir string) NodePolicy {
	body, err := os.ReadFile(NodePolicyPath(dir))
	if err != nil {
		return NodePolicy{}
	}
	var p NodePolicy
	if err := json.Unmarshal(body, &p); err != nil {
		return NodePolicy{}
	}
	return p
}
