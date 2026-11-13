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
	// backup-agent is node-local WORK, not cluster state: it takes this
	// node's snapshot and writes this node's artifacts, so it must run on a
	// worker that holds the data and has no current view of the cluster.
	// Gating it on leadership would be a failure mode, not a safety property.
	"backup-agent": scopeNode,
	// storage describes and provisions THIS machine's volume; there is no
	// cluster-wide answer and gating it on leadership would make a worker
	// unable to provision its own disk.
	"storage": scopeNode,
	// health-watch probes THIS machine's daemon and restarts THIS machine's
	// unit. It is the one command that MUST work on a worker with no cluster
	// state: a wedged worker daemon is exactly what it exists for, and gating
	// it on leadership would leave the node nobody can log into unwatched.
	"health-watch": scopeNode,

	// Bootstrap and lifecycle.
	"init":    scopeBootstrap,
	"setup":   scopeBootstrap,
	"join":    scopeBootstrap,
	"cluster": scopeLeader, // overridden per-subcommand below

	// Cluster state: leader only.
	//
	// doctor belongs here even though half of it is about this machine: the
	// secrets, the last apply and the config index all come from the local
	// store, and a follower's store is a stale copy. Answering "the platform is
	// healthy" from a node that cannot see the platform is the one wrong answer
	// this command must never give.
	//
	// It is EXEMPT from the refusal below (see enforceScope): a worker answers the
	// node-local subset instead — daemon, socket proxy, runner assignment, local
	// runner, disk — and prints one line naming each cluster question it did not
	// answer. Refusing outright is what left an operator unable to ask a worker
	// why its runner never started.
	"doctor":      scopeLeader,
	"backup":      scopeLeader,
	"config":      scopeLeader,
	"creds":       scopeLeader,
	"credentials": scopeLeader,
	"deploy":      scopeLeader,
	// github stores the App registration and the per-repo broker secrets in
	// the control-plane DB, and `github app test` mints against that
	// registration. A follower's copy is stale, so this is cluster state.
	"github":        scopeLeader,
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

// nodeLocalSubcommands overrides the top-level scope for subcommands that
// describe or act on THIS machine. `backup prune` deletes files in this node's
// archive directory: it has no cluster-wide answer, and gating it on leadership
// would leave a worker's disk unable to clean itself up.
var nodeLocalSubcommands = map[string]map[string]bool{
	"backup": {"prune": true},
}

// bootstrapClusterSubcommands are the `cluster` subcommands that must work
// anywhere: they bring a cluster into existence, tear it down without a store,
// or repair a node that cannot reach the control plane.
//
// `leader` belongs here because it does not read cluster state: it changes swarm
// membership, which any manager may do, and the operation that moves leadership
// AWAY from the current leader cannot by definition require running on it.
var bootstrapClusterSubcommands = map[string]bool{
	"up":     true, // creates the swarm and elects the first leader
	"reset":  true, // the escape hatch for a node that cannot reach the control plane
	"leader": true, // move leadership deliberately; needs a manager, not the leader
}

// mirrorReadable reports whether this invocation reads state a follower mirrors rather
// than state only the leader holds. Today that is exactly `cluster settings` with no
// argument or a `get`: its write form (`set`) is refused below.
//
// The check is on ARGS, not on the command, because both forms are the same cobra command:
// a scope table keyed by command name cannot tell them apart, and treating the whole
// command as readable would let a follower believe a write succeeded.
func mirrorReadable(cmd *cobra.Command, args []string) bool {
	if cmd.Name() != "settings" || cmd.Parent() == nil || cmd.Parent().Name() != "cluster" {
		return false
	}
	return len(args) == 0 || args[0] == "get"
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

	if subs, ok := nodeLocalSubcommands[name]; ok && cmd != top && subs[cmd.Name()] {
		return scopeNode
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
//
// args matter for one command: `cluster settings` READS what a follower mirrors, and
// only its write form needs the leader.
func enforceScope(cmd *cobra.Command, args []string) error {
	// Explicit remote mode: the user pointed the CLI at a daemon (usually the
	// edge, from their own machine). This is the only case where the CLI talks
	// HTTP, and it is always a deliberate choice.
	if apiURL != "" {
		return nil
	}
	if scopeOf(cmd) != scopeLeader {
		return nil
	}
	// `doctor` decides for itself: on a non-leader it answers the node-local subset
	// (daemon, socket proxy, runner assignment, local runner, disk, and since slice 3 of
	// ADR 0003 the MIRROR it holds, labelled with its age) and names what it will not
	// answer. The age is what keeps the guard's promise — no cluster state presented as
	// current from a node that does not hold it — while still answering.
	if cmd.Name() == "doctor" {
		return nil
	}

	// Reading cluster settings is the one cluster-state answer a follower can give: its
	// daemon mirrors the settings table and labels the answer with the leader and an age.
	// WRITING still needs the leader, because there is exactly one writer — and `set` is
	// the only form that writes, so the argument decides.
	if mirrorReadable(cmd, args) {
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
	if cmd.Name() == "settings" {
		b.WriteString("Reading is different: `pmcluster cluster settings` answers from this node's\n" +
			"mirror of the leader's settings, labelled with its age. Only WRITING needs the leader.\n\n")
	}

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
		return enforceScope(cmd, args)
	}
}
