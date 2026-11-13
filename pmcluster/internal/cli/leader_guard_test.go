package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
)

// stubSelf swaps the docker probe for the duration of a test.
func stubSelf(t *testing.T, self docker.SelfNode, err error) {
	t.Helper()
	prev := detectSelf
	detectSelf = func(context.Context) (docker.SelfNode, error) { return self, err }
	t.Cleanup(func() { detectSelf = prev })
}

// Every registered top-level command must be classified. This is the regression
// that matters: a new command that forgets to declare its scope would otherwise
// answer cluster state from whichever node it happens to run on.
func TestEveryTopLevelCommandHasAScope(t *testing.T) {
	for _, cmd := range rootCmd.Commands() {
		if cmd.Hidden || cmd.Name() == "help" || cmd.Name() == "completion" {
			continue
		}
		if _, ok := topLevelScopes[cmd.Name()]; !ok {
			t.Errorf("command %q has no entry in topLevelScopes — add it (scopeLeader unless it only describes this machine)", cmd.Name())
		}
	}
}

func TestScopeOf(t *testing.T) {
	find := func(name string) *cobra.Command {
		for _, c := range rootCmd.Commands() {
			if c.Name() == name {
				return c
			}
		}
		return nil
	}
	for name, want := range map[string]commandScope{
		"version":       scopeNode,
		"serve":         scopeNode,
		"init":          scopeBootstrap,
		"join":          scopeBootstrap,
		"setup":         scopeBootstrap,
		"stack":         scopeLeader,
		"secret":        scopeLeader,
		"config":        scopeLeader,
		"node":          scopeLeader,
		"observability": scopeLeader,
	} {
		cmd := find(name)
		if cmd == nil {
			t.Fatalf("command %q is not registered", name)
		}
		if got := scopeOf(cmd); got != want {
			t.Errorf("scopeOf(%s) = %v, want %v", name, got, want)
		}
	}

	// cluster: subcommands that bootstrap are allowed anywhere, the rest are not.
	cluster := find("cluster")
	if cluster == nil {
		t.Fatal("cluster command is not registered")
	}
	bootstrapSeen := 0
	for _, sub := range cluster.Commands() {
		want := scopeLeader
		if bootstrapClusterSubcommands[sub.Name()] {
			want = scopeBootstrap
			bootstrapSeen++
		}
		if got := scopeOf(sub); got != want {
			t.Errorf("scopeOf(cluster %s) = %v, want %v", sub.Name(), got, want)
		}
	}
	if bootstrapSeen == 0 {
		t.Error("no cluster subcommand was classified as bootstrap — the carve-outs are gone")
	}
}

// A follower must refuse cluster state, and say where to go instead.
func TestEnforceScope_RefusesOnNonLeader(t *testing.T) {
	t.Setenv("PMCLUSTER_API_URL", "")
	apiURL = ""
	stubSelf(t, docker.SelfNode{Hostname: "nxt-sw-3-w", Role: "worker", SwarmActive: true}, nil)

	err := enforceScope(stackListCmd, nil)
	if err == nil {
		t.Fatal("a worker must not answer stack state")
	}
	msg := err.Error()
	for _, want := range []string{"nxt-sw-3-w", "not the leader", "remote CLI", "--api-url"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not mention %q:\n%s", want, msg)
		}
	}
}

// A manager follower names the leader so the operator knows where to run it.
func TestEnforceScope_NamesTheLeader(t *testing.T) {
	apiURL = ""
	stubSelf(t, docker.SelfNode{Hostname: "nxt-sw-4-m", Role: "manager", SwarmActive: true, IsManager: true, LeaderHostname: "nxt-sw-1-m"}, nil)

	err := enforceScope(stackListCmd, nil)
	if err == nil {
		t.Fatal("a follower must not answer stack state")
	}
	if !strings.Contains(err.Error(), "nxt-sw-1-m") {
		t.Errorf("refusal should name the leader:\n%s", err)
	}
}

func TestEnforceScope_LeaderIsAllowed(t *testing.T) {
	apiURL = ""
	stubSelf(t, docker.SelfNode{Hostname: "nxt-sw-1-m", Role: "manager", SwarmActive: true, IsManager: true, IsLeader: true}, nil)

	if err := enforceScope(stackListCmd, nil); err != nil {
		t.Errorf("the leader must be allowed: %v", err)
	}
}

// Node-local and bootstrap commands never consult docker: they must work on a
// machine that has no swarm at all (install, join, recovery).
func TestEnforceScope_NodeAndBootstrapScopesSkipTheProbe(t *testing.T) {
	apiURL = ""
	stubSelf(t, docker.SelfNode{}, errors.New("docker is not installed"))

	for _, cmd := range []*cobra.Command{versionCmd, initCmd, joinCmd} {
		if err := enforceScope(cmd, nil); err != nil {
			t.Errorf("%s should not be leader-gated: %v", cmd.Name(), err)
		}
	}
	if err := enforceScope(clusterUpCmd, nil); err != nil {
		t.Errorf("cluster up is bootstrap and must run anywhere: %v", err)
	}
}

// Remote mode is the user's explicit choice on their own machine: the API decides
// scope, the node never does.
func TestEnforceScope_RemoteModeSkipsTheGuard(t *testing.T) {
	apiURL = "https://pmcluster.example.com"
	t.Cleanup(func() { apiURL = "" })
	stubSelf(t, docker.SelfNode{Hostname: "laptop", Role: "worker", SwarmActive: true}, nil)

	if err := enforceScope(stackListCmd, nil); err != nil {
		t.Errorf("remote mode must not be leader-gated: %v", err)
	}
}

// If docker cannot be reached, the refusal must still be honest and actionable.
func TestEnforceScope_ProbeFailureIsActionable(t *testing.T) {
	apiURL = ""
	stubSelf(t, docker.SelfNode{}, errors.New("Cannot connect to the Docker daemon"))

	err := enforceScope(stackListCmd, nil)
	if err == nil {
		t.Fatal("expected a refusal when leadership cannot be determined")
	}
	if !strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
		t.Errorf("the docker error should be preserved: %v", err)
	}
	if !strings.Contains(err.Error(), "--api-url") {
		t.Errorf("the refusal should offer the remote path: %v", err)
	}
}

// A node that is not in a swarm has no leader to defer to. This is how a cluster
// is created in the first place: `cluster up` on a bare node, and the setup
// wizard that `cluster update` falls back to when there is no data.
func TestEnforceScope_StandaloneNodeIsNotGated(t *testing.T) {
	apiURL = ""
	stubSelf(t, docker.SelfNode{Name: "fresh-host", Hostname: "fresh-host"}, nil)

	for _, cmd := range []*cobra.Command{stackListCmd, clusterUpdateCmd} {
		if err := enforceScope(cmd, nil); err != nil {
			t.Errorf("%s must run on a node with no swarm: %v", cmd.Name(), err)
		}
	}
}

// Participation in a swarm without leadership is the only state that refuses.
func TestEnforceScope_SwarmMemberWithoutLeadershipRefuses(t *testing.T) {
	apiURL = ""
	stubSelf(t, docker.SelfNode{Hostname: "nxt-sw-5-m", Role: "manager", SwarmActive: true, IsManager: true}, nil)

	err := enforceScope(stackListCmd, nil)
	if err == nil {
		t.Fatal("a swarm member that is not the leader must refuse cluster state")
	}
	if strings.Contains(err.Error(), "is a node,") {
		t.Errorf("the refusal should name a real role: %v", err)
	}
}
