package cli

import (
	"errors"
	"os"
	"testing"
)

// The timer must exec the platform's own binary, not the name the installer was
// invoked as. Installing from /usr/local/bin/pmcluster.new wrote that name into
// ExecStart and left the hourly run on a renamed copy of an older build.
func TestCanonicalAgentBinaryPrefersTheInstallPath(t *testing.T) {
	existing := func(string) (os.FileInfo, error) { return nil, nil }
	missing := func(string) (os.FileInfo, error) { return nil, errors.New("not found") }

	if got := canonicalAgentBinary("/usr/local/bin/pmcluster.new", existing); got != canonicalAgentPath {
		t.Fatalf("existing install path: got %q, want %q", got, canonicalAgentPath)
	}
	if got := canonicalAgentBinary("/opt/pmcluster", missing); got != "/opt/pmcluster" {
		t.Fatalf("no install path: got %q, want the invoked path", got)
	}
}
