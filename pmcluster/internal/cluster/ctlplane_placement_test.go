package cluster

import (
	"strings"
	"testing"
)

// The control-plane archiver must run where the control plane actually is. Pinned
// to the store node it archived an empty ~/.pmcluster for hours, and the resulting
// "backup" restored nothing.
func TestControlPlaneBackupRunsOnTheLeader(t *testing.T) {
	raw, err := LoadComposeFile(StackBackup, RenderInput{
		Domain:     "example.com",
		DataDir:    "/var/stack/data",
		LeaderNode: "leader-1",
		BackupNode: "worker-store",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := string(raw)
	if !strings.Contains(out, "control-plane-backup:") {
		t.Fatalf("control-plane-backup service missing:\n%s", out)
	}
	// With the store disabled in this render, the only hostname that may appear
	// is the leader's: the archiver reads ~/.pmcluster, which exists only there.
	if !strings.Contains(out, "node.hostname == leader-1") {
		t.Fatalf("control-plane archiver is not pinned to the leader:\n%s", out)
	}
	if strings.Contains(out, "worker-store") {
		t.Fatalf("control-plane archiver is pinned to the store node:\n%s", out)
	}
}
