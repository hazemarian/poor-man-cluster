package controlplane

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
)

// TestKitPaths pins where the two files live. The operator copies the key to KeyPath() and the
// daemon opens DBPath(); both hang off the single data dir it was given, and nothing else in
// the platform may invent a second location for either.
func TestKitPaths(t *testing.T) {
	dir := t.TempDir()
	kit := &Kit{DataDir: dir, Log: zerolog.Nop()}
	if got, want := kit.DBPath(), filepath.Join(dir, "data.db"); got != want {
		t.Errorf("DBPath() = %q, want %q", got, want)
	}
	if got, want := kit.KeyPath(), filepath.Join(dir, ".encryption_key"); got != want {
		t.Errorf("KeyPath() = %q, want %q", got, want)
	}
}

// TestIsSwarmUnavailable covers the messages that mean "ask again later" rather than "broken".
//
// The distinction is load-bearing: a daemon that treats a leaderless swarm as fatal exits,
// systemd restarts it, and every manager crash-loops while quorum recovers (seen live: 28
// restarts on one manager).
func TestIsSwarmUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"worker", errors.New("This node is not a swarm manager"), true},
		{"no leader", errors.New("rpc error: does not have a leader"), true},
		{"quorum", errors.New("too few managers are online"), true},
		{"docker gone", errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock"), true},
		{"socket gone", errors.New("connect: no such file or directory"), true},
		{"a real failure", errors.New("permission denied on /var/lib/pmcluster"), false},
	} {
		if got := IsSwarmUnavailable(tc.err); got != tc.want {
			t.Errorf("IsSwarmUnavailable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
