package cli

import (
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRenderSystemdUnit(t *testing.T) {
	unit := renderSystemdUnit("pmuser", "docker", "/home/pmuser", "/usr/local/bin/pmcluster serve")
	for _, want := range []string{
		"[Unit]",
		"Description=pmcluster API Server",
		"After=docker.service",
		"Requires=docker.service",
		"[Service]",
		"Type=simple",
		"User=pmuser",
		"Group=docker",
		"Environment=HOME=/home/pmuser",
		"ExecStart=/usr/local/bin/pmcluster serve",
		"Restart=always",
		"RestartSec=5",
		"[Install]",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit template missing %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "${") {
		t.Errorf("unit template contains unresolved variables:\n%s", unit)
	}
}

// Regression: the systemd unit must run `pmcluster serve`, never the bare
// binary. A bare ExecStart prints help and exits, crash-looping the service.
func TestDaemonExecStart_IncludesServe(t *testing.T) {
	if got := daemonExecStart("/usr/local/bin/pmcluster"); got != "/usr/local/bin/pmcluster serve" {
		t.Fatalf("daemonExecStart(/usr/local/bin/pmcluster) = %q, want %q", got, "/usr/local/bin/pmcluster serve")
	}
}

func TestJoinCommandRegistration(t *testing.T) {
	if joinCmd == nil {
		t.Fatal("joinCmd is nil")
	}
	if joinCmd.Use != "join" {
		t.Errorf("expected Use=join, got %q", joinCmd.Use)
	}
	if joinCmd.Short == "" {
		t.Error("joinCmd has no Short description")
	}
	// --token and --manager flags must exist.
	var hasToken, hasManager, hasHostname bool
	for _, f := range []string{"token", "manager", "hostname"} {
		if joinCmd.Flags().Lookup(f) != nil {
			switch f {
			case "token":
				hasToken = true
			case "manager":
				hasManager = true
			case "hostname":
				hasHostname = true
			}
		}
	}
	if !hasToken || !hasManager || !hasHostname {
		t.Errorf("joinCmd missing --token (hasToken=%v), --manager (hasManager=%v), or --hostname (hasHostname=%v)", hasToken, hasManager, hasHostname)
	}
}

// TestSetNodeHostname_RejectsGarbage asserts the hostname value is validated
// before it reaches hostnamectl (and later lands verbatim in Swarm constraint
// expressions and the node list match).
func TestSetNodeHostname_RejectsGarbage(t *testing.T) {
	for _, bad := range []string{"bad host!", "has/slash", "quote\"d", "a b", "$(rm)", "new\nline"} {
		if err := setNodeHostname(t.Context(), io.Discard, bad); err == nil {
			t.Errorf("expected error for hostname %q, got nil", bad)
		}
	}
}

func TestJoinRoleFlagValidated(t *testing.T) {
	// --role must be worker or manager.
	for _, bad := range []string{"master", "", "swarm"} {
		c := setupJoinTestCmd()
		c.Flags().Set("role", bad)
		c.Flags().Set("token", "SWMTKN-1-x")
		c.Flags().Set("manager", "10.0.0.5:2377")
		err := runJoin(c, nil)
		if err == nil {
			t.Fatalf("expected error for --role %q, got nil", bad)
		}
		if !strings.Contains(err.Error(), "--role must be 'worker' or 'manager'") {
			t.Fatalf("unexpected error for --role %q: %v", bad, err)
		}
	}
}

func TestJoinRequiresTokenAndManager(t *testing.T) {
	c := setupJoinTestCmd()
	c.Flags().Set("role", "worker")
	c.Flags().Set("token", "")
	c.Flags().Set("manager", "10.0.0.5:2377")
	if err := runJoin(c, nil); err == nil || !strings.Contains(err.Error(), "--token") {
		t.Fatalf("expected --token error, got %v", err)
	}
	c = setupJoinTestCmd()
	c.Flags().Set("role", "worker")
	c.Flags().Set("token", "SWMTKN-1-x")
	c.Flags().Set("manager", "")
	if err := runJoin(c, nil); err == nil || !strings.Contains(err.Error(), "--manager") {
		t.Fatalf("expected --manager error, got %v", err)
	}
}

// setupJoinTestCmd returns a joinCmd clone whose flags are reset.
func setupJoinTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "join"}
	c.Flags().String("token", "", "")
	c.Flags().String("manager", "", "")
	c.Flags().String("role", "worker", "")
	return c
}
