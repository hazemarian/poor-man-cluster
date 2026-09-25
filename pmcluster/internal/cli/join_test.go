package cli

import (
	"strings"
	"testing"
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
	var hasToken, hasManager bool
	for _, f := range []string{"token", "manager"} {
		if joinCmd.Flags().Lookup(f) != nil {
			if f == "token" {
				hasToken = true
			} else {
				hasManager = true
			}
		}
	}
	if !hasToken || !hasManager {
		t.Errorf("joinCmd missing --token (hasToken=%v) or --manager (hasManager=%v)", hasToken, hasManager)
	}
}
