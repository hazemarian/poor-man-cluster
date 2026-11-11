package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runners"
)

type stubRunners struct {
	rep runners.Report
	err error
}

func (s stubRunners) Inspect(context.Context) (runners.Report, error) { return s.rep, s.err }

func runRunnerCheck(t *testing.T, probe RunnerProbe) Check {
	t.Helper()
	var found *Check
	for _, c := range mustReport(t, Input{GitHubRunners: probe}).Checks {
		if c.Name == "github runners" {
			cc := c
			found = &cc
		}
	}
	if found == nil {
		t.Fatal("the report has no \"github runners\" check")
	}
	return *found
}

func mustReport(t *testing.T, in Input) *Report {
	t.Helper()
	rep, err := Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rep
}

func TestGitHubRunnersCheckSkippedWithoutAManager(t *testing.T) {
	c := runRunnerCheck(t, nil)
	if c.Status != StatusSkip {
		t.Fatalf("status = %s, want skip (no manager to inspect)", c.Status)
	}
}

func TestGitHubRunnersCheckDisabledIsOK(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{rep: runners.Report{Enabled: false}})
	if c.Status != StatusOK || !strings.Contains(c.Detail, "disabled") {
		t.Fatalf("check = %+v, want ok/disabled", c)
	}
}

// DISABLED first, and leftovers are the failure mode of a half-disable: a
// container or a registration that outlived the switch is a warning naming the
// setting, not a silent "disabled, all good".
func TestGitHubRunnersCheckWarnsAboutLeftoversWhileDisabled(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{rep: runners.Report{
		Enabled:   false,
		Leftovers: []string{"container pmc-worker-1-org (running)"},
	}})
	if c.Status != StatusWarn {
		t.Fatalf("status = %s, want warn", c.Status)
	}
	for _, want := range []string{"github_runners_enabled", "container pmc-worker-1-org"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q does not mention %q", c.Detail, want)
		}
	}
}

func TestGitHubRunnersCheckFailsWithoutAnApp(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{rep: runners.Report{
		Enabled: true, Org: "acme", Group: "pmcluster",
		Nodes: []runners.NodeReport{{Node: "worker-1"}},
	}})
	if c.Status != StatusFail {
		t.Fatalf("status = %s, want fail", c.Status)
	}
	if !strings.Contains(c.Detail, "GitHub App") {
		t.Fatalf("detail = %q, want it to name the App requirement", c.Detail)
	}
}

// An enabled feature with no node selected is a refusal, not a warning: nothing
// runs anywhere and the operator has to know.
func TestGitHubRunnersCheckFailsWithoutASelectedNode(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{rep: runners.Report{
		Enabled: true, AppConfigured: true, Org: "acme", Group: "pmcluster",
		Warnings: []string{"github_runner_nodes is empty, so no runner will run anywhere"},
	}})
	if c.Status != StatusFail {
		t.Fatalf("status = %s, want fail", c.Status)
	}
	if !strings.Contains(c.Detail, "github_runner_nodes") {
		t.Fatalf("detail = %q, want it to name the setting", c.Detail)
	}
}

func TestGitHubRunnersCheckFailsOnMissingSelfHostedRunnersWrite(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{rep: runners.Report{
		Enabled:           true,
		AppConfigured:     true,
		Org:               "acme",
		Group:             "pmcluster",
		MissingPermission: runners.RequiredPermission,
		Nodes:             []runners.NodeReport{{Node: "worker-1"}},
	}})
	if c.Status != StatusFail {
		t.Fatalf("status = %s, want fail", c.Status)
	}
	for _, want := range []string{"organization_self_hosted_runners:write", "Self-hosted runners", "Organization"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q does not mention %q", c.Detail, want)
		}
	}
}

func TestGitHubRunnersCheckHealthy(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{rep: runners.Report{
		Enabled:       true,
		AppConfigured: true,
		Org:           "acme",
		Group:         "pmcluster",
		Nodes: []runners.NodeReport{{
			Node: "worker-1", Role: "worker",
			RunnerName: "pmc-worker-1-org", ContainerRunning: true, ContainerKnown: true, Registered: true,
			DockerAccess: "socket proxy",
		}},
	}})
	if c.Status != StatusOK {
		t.Fatalf("check = %+v, want ok", c)
	}
	for _, want := range []string{"enabled", "1 runner", "socket proxy"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q does not mention %q", c.Detail, want)
		}
	}
}

func TestGitHubRunnersCheckNamesOfflineAndGhostRunners(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{rep: runners.Report{
		Enabled:       true,
		AppConfigured: true,
		Org:           "acme",
		Group:         "pmcluster",
		Nodes: []runners.NodeReport{
			{Node: "worker-1", Role: "worker", RunnerName: "pmc-worker-1-org", ContainerRunning: true, Registered: true,
				Offline: []string{"pmc-worker-1-org"}},
			// ContainerKnown: this daemon looked at its own node and found the
			// container running while GitHub has no such registration.
			{Node: "worker-2", Role: "worker", RunnerName: "pmc-worker-2-org", Registered: false, ContainerRunning: true, ContainerKnown: true},
			{Node: "worker-3", Role: "worker", RunnerName: "pmc-worker-3-org", Failures: 4, LastError: "403"},
		},
	}})
	if c.Status != StatusWarn {
		t.Fatalf("status = %s, want warn", c.Status)
	}
	for _, want := range []string{"pmc-worker-1-org", "worker-2", "4 consecutive"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q does not mention %q", c.Detail, want)
		}
	}
}

// A runner on a manager (or the leader) is the failure that took the live cluster
// down with the telemetry loop: a build there marks the node Down.
func TestGitHubRunnersCheckWarnsAboutARunnerOnAManager(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{rep: runners.Report{
		Enabled:       true,
		AppConfigured: true,
		Org:           "acme",
		Group:         "pmcluster",
		Nodes: []runners.NodeReport{{
			Node: "manager-1", Role: "leader", RunnerName: "pmc-manager-1-org",
			ContainerRunning: true, Registered: true,
		}},
	}})
	if c.Status != StatusWarn {
		t.Fatalf("status = %s, want warn", c.Status)
	}
	if !strings.Contains(c.Detail, "Down") {
		t.Fatalf("detail = %q, want the node-Down reasoning", c.Detail)
	}
}

// The filtered socket is the default; a runner holding the raw socket is worth a
// warning even when everything else agrees.
func TestGitHubRunnersCheckWarnsAboutTheRawSocket(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{rep: runners.Report{
		Enabled:       true,
		AppConfigured: true,
		Org:           "acme",
		Group:         "pmcluster",
		Nodes: []runners.NodeReport{{
			Node: "worker-1", Role: "worker", RunnerName: "pmc-worker-1-org",
			ContainerRunning: true, Registered: true, RawSocket: true, DockerAccess: "raw socket",
		}},
	}})
	if c.Status != StatusWarn {
		t.Fatalf("status = %s, want warn", c.Status)
	}
	if !strings.Contains(c.Detail, "github_runner_socket_proxy") {
		t.Fatalf("detail = %q, want it to name the socket-proxy setting", c.Detail)
	}
}

// The operator's hand-made runner is named and never counted as healthy: the
// platform's view and reality differ, and hiding that is how a stray root-mounted
// socket survives a cleanup.
func TestGitHubRunnersCheckNamesTheUnmanagedRunner(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{rep: runners.Report{
		Enabled:       true,
		AppConfigured: true,
		Org:           "acme",
		Group:         "pmcluster",
		Nodes: []runners.NodeReport{{
			Node: "worker-1", Role: "worker", RunnerName: "pmc-worker-1-org",
			ContainerRunning: true, Registered: true,
		}},
		Unmanaged: []runners.RunnerNote{{Name: "pmc-org-runner", SocketKnown: false}},
	}})
	if c.Status != StatusWarn {
		t.Fatalf("status = %s, want warn (an unmanaged runner is not healthy)", c.Status)
	}
	if !strings.Contains(c.Detail, "pmc-org-runner") {
		t.Fatalf("detail = %q, want the runner named", c.Detail)
	}
	if strings.Contains(c.Detail, "1 runner(s) registered and running") {
		t.Fatalf("detail claims a healthy count while an unmanaged runner exists: %q", c.Detail)
	}
}

func TestGitHubRunnersCheckInspectErrorFails(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{err: context.DeadlineExceeded})
	if c.Status != StatusFail {
		t.Fatalf("status = %s, want fail", c.Status)
	}
}

// TestGitHubRunnersCheckDoesNotInventProblemsForRemoteNodes is the regression for
// a fleet-wide false positive: ContainerRunning is only observable on the node
// the daemon runs on, so from the leader every worker carried the zero value and
// doctor printed "registered but no container is running" for a perfectly healthy
// runner. An unknown container state must stay silent — the worker's own doctor
// is the one that can see it.
func TestGitHubRunnersCheckDoesNotInventProblemsForRemoteNodes(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{rep: runners.Report{
		Enabled:       true,
		AppConfigured: true,
		Org:           "acme",
		Group:         "pmcluster",
		Nodes: []runners.NodeReport{{
			Node: "worker-1", Role: "worker",
			RunnerName: "pmc-worker-1-org",
			Registered: true, // GitHub sees it…
			// …and we cannot see its container from here: no ContainerKnown.
		}},
	}})
	if c.Status != StatusOK {
		t.Fatalf("status = %s, want ok — an unobservable container is not a fault: %s", c.Status, c.Detail)
	}
	if strings.Contains(c.Detail, "no container") {
		t.Fatalf("detail invents a missing container: %q", c.Detail)
	}
}

// TestGitHubRunnersCheckDoesNotCallRegistrationRunning: "registered and running" is
// a claim about containers. This daemon only sees containers on its own node, so
// from the leader the old wording said "2 runner(s) registered and running" about a
// node with no container at all — while the runner was still pulling its image, or
// never started. The detail must state what was observed.
func TestGitHubRunnersCheckDoesNotCallRegistrationRunning(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{rep: runners.Report{
		Enabled:       true,
		AppConfigured: true,
		Org:           "acme",
		Group:         "pmcluster",
		Nodes: []runners.NodeReport{{
			Node: "worker-1", Role: "worker", RunnerName: "pmc-worker-1-org",
			Registered: true,
			// ContainerKnown=false: this daemon cannot see worker-1's containers.
		}},
	}})
	if c.Status != StatusOK {
		t.Fatalf("status = %s, want ok: %s", c.Status, c.Detail)
	}
	if strings.Contains(c.Detail, "registered and running") {
		t.Fatalf("the detail claims a container it cannot see: %q", c.Detail)
	}
	if !strings.Contains(c.Detail, "none confirmed running") {
		t.Fatalf("the detail does not say the container is unconfirmed: %q", c.Detail)
	}
}

// …and when the container WAS observed running, the strong claim is correct.
func TestGitHubRunnersCheckSaysRunningWhenItSawIt(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{rep: runners.Report{
		Enabled:       true,
		AppConfigured: true,
		Org:           "acme",
		Group:         "pmcluster",
		Nodes: []runners.NodeReport{{
			Node: "worker-1", Role: "worker", RunnerName: "pmc-worker-1-org",
			Registered: true, ContainerKnown: true, ContainerRunning: true,
		}},
	}})
	if !strings.Contains(c.Detail, "registered and running") {
		t.Fatalf("an observed container is not reported as running: %q", c.Detail)
	}
}
