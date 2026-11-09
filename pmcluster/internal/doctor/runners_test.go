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

func TestGitHubRunnersCheckFailsWithoutAnApp(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{rep: runners.Report{Enabled: true}})
	if c.Status != StatusFail {
		t.Fatalf("status = %s, want fail", c.Status)
	}
	if !strings.Contains(c.Detail, "GitHub App") {
		t.Fatalf("detail = %q, want it to name the App requirement", c.Detail)
	}
}

func TestGitHubRunnersCheckFailsOnMissingAdministrationWrite(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{rep: runners.Report{
		Enabled:           true,
		AppConfigured:     true,
		MissingPermission: runners.RequiredPermission,
		Repos:             []runners.RepoReport{{Repo: "acme/api"}},
	}})
	if c.Status != StatusFail {
		t.Fatalf("status = %s, want fail", c.Status)
	}
	for _, want := range []string{"administration:write", "Administration: Read & write"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q does not mention %q", c.Detail, want)
		}
	}
}

func TestGitHubRunnersCheckWarnsOnEmptyRepoList(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{rep: runners.Report{Enabled: true, AppConfigured: true}})
	if c.Status != StatusWarn {
		t.Fatalf("status = %s, want warn", c.Status)
	}
}

func TestGitHubRunnersCheckHealthy(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{rep: runners.Report{
		Enabled:       true,
		AppConfigured: true,
		Repos: []runners.RepoReport{{
			Repo: "acme/api", RunnerName: "pmc-node1-acme-api",
			ContainerRunning: true, Registered: true,
		}},
	}})
	if c.Status != StatusOK {
		t.Fatalf("check = %+v, want ok", c)
	}
	if !strings.Contains(c.Detail, "1 runner") {
		t.Fatalf("detail = %q", c.Detail)
	}
}

func TestGitHubRunnersCheckNamesOfflineAndGhostRunners(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{rep: runners.Report{
		Enabled:       true,
		AppConfigured: true,
		Repos: []runners.RepoReport{
			{Repo: "acme/api", RunnerName: "pmc-node1-acme-api", ContainerRunning: true, Registered: true,
				Offline: []string{"pmc-node0-acme-api"}},
			{Repo: "acme/web", RunnerName: "pmc-node1-acme-web", Registered: false, ContainerRunning: true},
			{Repo: "acme/db", RunnerName: "pmc-node1-acme-db", Failures: 4, LastError: "403"},
		},
	}})
	if c.Status != StatusWarn {
		t.Fatalf("status = %s, want warn", c.Status)
	}
	for _, want := range []string{"pmc-node0-acme-api", "acme/web", "4 consecutive"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q does not mention %q", c.Detail, want)
		}
	}
}

func TestGitHubRunnersCheckInspectErrorFails(t *testing.T) {
	c := runRunnerCheck(t, stubRunners{err: context.DeadlineExceeded})
	if c.Status != StatusFail {
		t.Fatalf("status = %s, want fail", c.Status)
	}
}
