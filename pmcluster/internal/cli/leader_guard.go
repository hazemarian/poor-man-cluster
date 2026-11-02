package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
)

// Where a command's answer is authoritative. The leader is the only node whose
// view of cluster state is current, and the daemon already works this way: a
// non-leader manager runs as a standby that does NOT serve, polling until swarm
// elects it leader. The CLI used to be the one component on a non-leader that
// still opened the local store and answered from it — which is how a worker came
// to report "no stacks yet" while ten stacks were running.
type commandScope int

const (
	// scopeNode: node-local facts. Correct on any node, because they describe
	// the machine the command is running on and nothing else.
	scopeNode commandScope = iota
	// scopeBootstrap: pre-cluster and radar-up. These cannot be leader-gated:
	// `join` runs on a node that is not in the cluster yet, `cluster up` is what
	// elects the first leader, and `serve` is how a standby waits to become one.
	scopeBootstrap
	// scopeLeader: cluster state — stacks, secrets, configs, credentials, nodes,
	// settings, telemetry, TLS, registries, webhooks, users. Only the leader
	// holds a current copy.
	scopeLeader
)

// topLevelScopes is the classification table. Every top-level command must
// appear here; a test enforces that, so a new command cannot silently inherit
// "answers cluster state from a node that does not have it".
var topLevelScopes = map[string]commandScope{
	// Node-local: describes this machine, nothing else.
	"version": scopeNode,
	"serve":   scopeNode, // standby on a non-leader by design

	// Bootstrap and lifecycle.
	"init":    scopeBootstrap,
	"setup":   scopeBootstrap,
	"join":    scopeBootstrap,
	"cluster": scopeLeader, // overridden per-subcommand below

	// Cluster state: leader only.
	"backup":        scopeLeader,
	"config":        scopeLeader,
	"creds":         scopeLeader,
	"credentials":   scopeLeader,
	"deploy":        scopeLeader,
	"logs":          scopeLeader,
	"node":          scopeLeader,
	"observability": scopeLeader,
	"registry":      scopeLeader,
	"rollback":      scopeLeader,
	"secret":        scopeLeader,
	"service":       scopeLeader,
	"stack":         scopeLeader,
	"tls":           scopeLeader,
	"usage":         scopeLeader,
	"user":          scopeLeader,
	"webhook":       scopeLeader,
}

// bootstrapClusterSubcommands are the `cluster` subcommands that must work
// anywhere: they bring a cluster into existence, tear it down without a store,
// or repair a node that cannot reach the control plane.
var bootstrapClusterSubcommands = map[string]bool{
	"up":    true, // creates the swarm and elects the first leader
	"reset": true, // the escape hatch for a node that cannot reach the control plane
}

// detectSelf is a seam for tests; production reads the local docker socket.
var detectSelf = docker.InspectSelf

// scopeOf resolves a command to its scope by its top-level name. Unknown
// commands default to scopeLeader: refusing with a clear message is better than
// answering cluster state from the wrong node.
func scopeOf(cmd *cobra.Command) commandScope {
	top := cmd
	for top.HasParent() && top.Parent().HasParent() {
		top = top.Parent()
	}
	name := top.Name()

	if name == "cluster" && cmd != top {
		if bootstrapClusterSubcommands[cmd.Name()] {
			return scopeBootstrap
		}
		return scopeLeader
	}

	if s, ok := topLevelScopes[name]; ok {
		return s
	}
	return scopeLeader
}

// enforceScope is the guard wired into the root command's PersistentPreRunE. It
// runs BEFORE any command body, so a refusal happens before the store is opened —
// opening it is itself what seeded an empty database on every worker and made the
// empty answer look plausible.
func enforceScope(cmd *cobra.Command) error {
	// Explicit remote mode: the user pointed the CLI at a daemon (usually the
	// edge, from their own machine). This is the only case where the CLI talks
	// HTTP, and it is always a deliberate choice.
	if apiURL != "" {
		return nil
	}
	if scopeOf(cmd) != scopeLeader {
		return nil
	}

	self, err := detectSelf(cmd.Context())
	if err != nil {
		return fmt.Errorf("this command needs the cluster leader, and this machine cannot be asked whether it is one: %w\n\n"+
			"Nothing has been read or changed. Run it on the leader, or use the remote CLI:\n  %s",
			err, remoteHint(cmd, ""))
	}
	// Standalone/local mode: this node is not in a swarm, so there is no leader to
	// defer to and nothing to be stale against. The daemon serves in this state
	// too ("serving (standalone/local mode)"), and bootstrap depends on it.
	if !self.SwarmActive || self.IsLeader {
		return nil
	}
	return notLeader(cmd, self)
}

// notLeader explains the refusal and gives the operator both ways out: go to the
// leader, or point the CLI at the API from a machine of their own.
func notLeader(cmd *cobra.Command, self docker.SelfNode) error {
	node := self.Hostname
	if node == "" {
		node = "this node"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s is a %s, not the leader — cluster state is only current on the leader.\n", node, orDefault(self.Role, "node"))
	if self.LeaderHostname != "" {
		fmt.Fprintf(&b, "\n  leader : %s\n", self.LeaderHostname)
	} else {
		b.WriteString("\n  leader : (unknown from a worker — `docker node ls` is a manager-only call)\n")
	}
	b.WriteString("\nThis node holds no authoritative copy of stacks, secrets, configs, credentials,\n" +
		"nodes or settings. The command is refused rather than answered from an empty or\n" +
		"stale local store:\n\n")
	fmt.Fprintf(&b, "  %s\n\n", cmd.CommandPath())

	b.WriteString("Run it on the leader, or use the remote CLI from your own machine:\n")
	fmt.Fprintf(&b, "  %s\n", remoteHint(cmd, leaderURL(self)))
	return fmt.Errorf("%s", b.String())
}

// remoteHint renders a copy-pasteable remote invocation. The URL is the edge when
// the node knows the cluster domain, otherwise the leader's API address.
func remoteHint(cmd *cobra.Command, base string) string {
	args := strings.TrimPrefix(cmd.CommandPath(), "pmcluster")
	if base == "" {
		base = "<leader-or-edge-url>"
	}
	return fmt.Sprintf("pmcluster%s --api-url %s --api-token <token>", args, base)
}

// leaderURL prefers the cluster's public edge address (stable across elections)
// and falls back to empty when this node cannot know the domain.
func leaderURL(self docker.SelfNode) string {
	if domain := clusterDomainFromConfig(); domain != "" {
		return "https://pmcluster." + domain
	}
	return ""
}

// clusterDomainFromConfig reads the domain from the local config file when one is
// present. It never fails: the hint is cosmetic, and a missing config must not
// turn a refusal into a different error.
func clusterDomainFromConfig() string {
	path := configPath
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		path = home + "/.pmcluster/config.yaml"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "domain:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// withScopeGuard installs the guard on the root command. Called from init().
func withScopeGuard() {
	prev := rootCmd.PersistentPreRunE
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if prev != nil {
			if err := prev(cmd, args); err != nil {
				return err
			}
		}
		return enforceScope(cmd)
	}
}
