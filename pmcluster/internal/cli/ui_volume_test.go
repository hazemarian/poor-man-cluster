package cli

import (
	"strings"
	"testing"
)

// TestContainerArgsMountTheVolumeAsTheConsole — the break-glass path must reach the
// console's named volume the same way the console does, as the console's uid, and
// it must not recurse into another container.
func TestContainerArgsMountTheVolumeAsTheConsole(t *testing.T) {
	got := containerArgs("edge_pmui-data", "10001", "/usr/local/bin/pmcluster", "alpine:3.20",
		[]string{"ui", "approve", "who@example.com", "--role", "admin"})

	joined := strings.Join(got, " ")
	for _, want := range []string{
		"-v edge_pmui-data:/data",                 // the volume, by name
		"-u 10001",                                // the console's uid, not root
		"/usr/local/bin/pmcluster:ro",             // our own binary, read-only
		"ui approve who@example.com --role admin", // the operator's command
		"--data-dir /data",                        // where the volume is inside
		"--volume  --in-volume",                   // no recursion
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker argv missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "/var/lib/docker/volumes") {
		t.Errorf("argv reaches into the host path instead of the volume:\n%s", joined)
	}
}

// TestForwardArgsDropsThisCommandsPlumbing — only the operator's own words travel,
// so the child cannot be handed a conflicting --data-dir or --volume.
func TestForwardArgsDropsThisCommandsPlumbing(t *testing.T) {
	in := []string{"ui", "approve", "a@b.c", "--role", "admin", "--volume", "other", "--data-dir", "/x", "--uid=5", "--in-volume", "--image", "img"}
	got := strings.Join(forwardArgs(in), " ")
	want := "ui approve a@b.c --role admin"
	if got != want {
		t.Fatalf("forwarded = %q, want %q", got, want)
	}
}
