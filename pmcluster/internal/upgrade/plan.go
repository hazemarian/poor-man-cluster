// Package upgrade replaces the pmcluster binary across a swarm — as a workflow, not a script.
//
// Why this is not a shell script any more. `hack/upgrade-nodes.sh` ordered the nodes correctly,
// verified the checksum, and treated every step as fatal, which is most of the way there. What
// it could not do was VERIFY: it slept six seconds after each restart and printed
// `systemctl is-active`, which reads "active" for a binary that starts and dies in a loop often
// enough to matter, and it never rolled back. So a bad release reported success on every node
// and left a crash-looping fleet, discovered by whoever hit the API next. That is the one
// operation that runs unattended and can take everything down; it deserves the same shape as
// `cluster update`: named phases, a gate after every mutation, and a refusal instead of a
// best-effort.
//
// The transport is an interface on purpose. Today it is SSH (a node cannot be asked to replace
// its own binary over the API it is serving, and a leader cannot orchestrate its own restart);
// the workflow below does not know that, so moving the last hop to mTLS/node API later is a new
// implementation, not a rewrite.
package upgrade

import (
	"fmt"
	"strings"
)

// Target is one node to upgrade.
type Target struct {
	// Host is the address the transport reaches this node by (a public IP in practice).
	Host string
	// User is the login the transport uses.
	User string
	// Role is the swarm role as reported by docker: "manager" or "worker".
	Role string
	// Leader marks the node the swarm elected.
	Leader bool
}

// Plan orders the targets for a rollout: workers first, then follower managers, and the leader
// LAST — the leader is the only node whose restart can cost the swarm its quorum chair and its
// API, so it goes when every other node already runs the new binary and can answer.
//
// It refuses when no node claims leadership. A rollout that cannot identify the leader is a
// rollout that will guess, and guessing wrong means restarting the leader in the middle of the
// sequence — which is what this ordering exists to prevent. (The old script learned this the
// hard way: matching a public-IP list against the swarm's internal addresses matched nothing and
// silently kept the typed order, so "leader last" was void without a word.)
func Plan(targets []Target) ([]Target, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("no nodes to upgrade")
	}
	var workers, managers, leaders []Target
	for _, t := range targets {
		switch {
		case t.Leader:
			leaders = append(leaders, t)
		case strings.EqualFold(t.Role, "manager"):
			managers = append(managers, t)
		default:
			workers = append(workers, t)
		}
	}
	if len(leaders) == 0 {
		return nil, fmt.Errorf("no node claims leadership — refusing to guess an upgrade order (%d node(s) seen: %s); pass an explicit order only if you know the leader is not in this list",
			len(targets), hostList(targets))
	}
	if len(leaders) > 1 {
		// Two leaders is a split brain or a stale node list; either way the ordering rule
		// ("the leader last") has no single meaning.
		return nil, fmt.Errorf("%d nodes claim leadership (%s) — refusing: a rollout needs exactly one leader last",
			len(leaders), hostList(leaders))
	}
	out := make([]Target, 0, len(targets))
	out = append(out, workers...)
	out = append(out, managers...)
	out = append(out, leaders...)
	return out, nil
}

func hostList(ts []Target) string {
	hosts := make([]string, 0, len(ts))
	for _, t := range ts {
		hosts = append(hosts, t.Host)
	}
	return strings.Join(hosts, ",")
}
