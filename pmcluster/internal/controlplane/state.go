// Package controlplane answers the leadership-gain question about this node's own store
// (see adopt.go). It no longer writes, publishes or reads a Docker config.
//
// What used to be here, and why it is not. ADR 0005 retired the data kit — the mirror
// carries the whole control-plane document and adoption copies nothing — and this
// release retires the key config that outlived it. The cipher key is minted once at
// `cluster init`, it never rotates, and it is a FILE: `<data dir>/.encryption_key`. Today
// an operator copies it to each manager; the join route (`POST /internal/api/join/key`,
// keyed by swarm mTLS) will do the same thing automatically when it lands. A key that never
// changes needs a place to live, not a delivery channel.
package controlplane

import (
	"path/filepath"
	"strings"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
)

// IsSwarmUnavailable reports whether err is a *transient Swarm availability*
// error rather than a genuine control-plane failure. Two cases qualify:
//
//   - this node is a worker ("This node is not a swarm manager") — swarm
//     configs are manager-only, so the replicated key is invisible here;
//   - the swarm is momentarily without a Raft leader ("does not have a
//     leader" / "too few managers are online") — quorum was lost, so even a
//     manager cannot read the config *right now*.
//
// Both mean the key config cannot be read at this instant, so callers degrade and retry
// later instead of failing fatally. That matters most when a daemon (re)starts during a
// quorum outage: without this it exits, systemd restarts it, and every manager crash-loops
// (seen live: a manager restarted 28 times while the swarm had no leader).
// The matched strings are stable Docker messages (daemon/cluster/errors.go
// errSwarmNotManager; SwarmKit raft errNoLeader).
func IsSwarmUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "is not a swarm manager") ||
		strings.Contains(msg, "does not have a leader") ||
		strings.Contains(msg, "too few managers are online") ||
		// A dead or wedged dockerd: the swarm is unreadable for exactly the same
		// reason a leaderless one is — this node cannot ask it anything. Treating it
		// as a control-plane failure made the daemon exit, so systemd restart-loop
		// it to `failed` on the node whose docker had just died, which is the one
		// node that had to stay up long enough for the docker watchdog to fire
		// (measured on nxt-sw-6-w, 2026-11-13).
		isDockerDaemonUnavailable(msg)
}

// isDockerDaemonUnavailable matches the client's own wording for "there is no
// daemon to talk to": an absent socket, a refused connection, or a daemon that
// accepted the call and never answered.
func isDockerDaemonUnavailable(msg string) bool {
	for _, s := range []string{
		"Cannot connect to the Docker daemon",
		"Is the docker daemon running",
		"connect: no such file or directory",
		"connect: connection refused",
		"context deadline exceeded",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// Kit answers "may this node serve after a leadership gain" for one daemon (see
// adopt.go). Create one per daemon; it holds no per-process state.
type Kit struct {
	// DataDir is where this node keeps data.db and its encryption key.
	DataDir string
	// Cipher decrypts the store's secrets. Built from the key FILE, so a node whose
	// key is missing never gets one of these — which is the point (ADR 0006).
	Cipher *credentials.Cipher
	// Standalone marks a node that is its own cluster: no swarm, so no peer, no mirror,
	// and nothing to vouch for. It adopts itself.
	Standalone bool
	Log        zerolog.Logger
}

// DBPath returns the SQLite database path inside DataDir.
func (k *Kit) DBPath() string { return filepath.Join(k.DataDir, "data.db") }

// KeyPath returns the encryption key path inside DataDir.
func (k *Kit) KeyPath() string { return filepath.Join(k.DataDir, ".encryption_key") }
