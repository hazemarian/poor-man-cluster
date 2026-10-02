package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeTailscaleExec is a recording tailscaleExecFn: it answers 'ip -4' with the
// configured address and records every invocation.
func fakeTailscaleExec(ip string, upErr error) (func(ctx context.Context, args ...string) ([]byte, error), *[]string) {
	var calls []string
	return func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if len(args) > 0 && args[0] == "up" && upErr != nil {
			return []byte("tailscale up: no such host"), upErr
		}
		if len(args) > 0 && args[0] == "ip" && args[1] == "-4" {
			return []byte(ip + "\n"), nil
		}
		return nil, nil
	}, &calls
}

func TestTailscaleUp_JoinsTailnet(t *testing.T) {
	fn, calls := fakeTailscaleExec("", nil)
	old := tailscaleExecFn
	tailscaleExecFn = fn
	defer func() { tailscaleExecFn = old }()

	var buf bytes.Buffer
	if err := tailscaleUp(context.Background(), &buf, "tskey-fake-123", "node-01"); err != nil {
		t.Fatalf("tailscaleUp: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0] != "up --auth-key=tskey-fake-123 --hostname=node-01" {
		t.Errorf("unexpected tailscale calls: %v", *calls)
	}
	if !strings.Contains(buf.String(), "✔ node joined the tailnet.") {
		t.Errorf("missing success message: %q", buf.String())
	}
}

func TestTailscaleUp_NoAuthKeySkipsFlag(t *testing.T) {
	fn, calls := fakeTailscaleExec("", nil)
	old := tailscaleExecFn
	tailscaleExecFn = fn
	defer func() { tailscaleExecFn = old }()

	if err := tailscaleUp(context.Background(), &bytes.Buffer{}, "", "node-01"); err != nil {
		t.Fatalf("tailscaleUp: %v", err)
	}
	if (*calls)[0] != "up --hostname=node-01" {
		t.Errorf("expected bare `up --hostname=node-01`, got %q", (*calls)[0])
	}
}

func TestTailscaleUp_FailureSurfaces(t *testing.T) {
	fn, _ := fakeTailscaleExec("", errors.New("up failed"))
	old := tailscaleExecFn
	tailscaleExecFn = fn
	defer func() { tailscaleExecFn = old }()

	if err := tailscaleUp(context.Background(), &bytes.Buffer{}, "tskey-x", ""); err == nil {
		t.Fatal("expected tailscaleUp error when `tailscale up` fails")
	} else if !strings.Contains(err.Error(), "tailscale up failed") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestTailscaleIP4_ParsesFirstLine(t *testing.T) {
	fn, _ := fakeTailscaleExec("100.64.0.5\n100.64.0.6\n", nil)
	old := tailscaleExecFn
	tailscaleExecFn = fn
	defer func() { tailscaleExecFn = old }()

	ip, err := tailscaleIP4(context.Background())
	if err != nil {
		t.Fatalf("tailscaleIP4: %v", err)
	}
	if ip != "100.64.0.5" {
		t.Errorf("tailscaleIP4 = %q, want first line 100.64.0.5", ip)
	}
}

func TestTailscaleIP4_EmptyAddressErrors(t *testing.T) {
	fn, _ := fakeTailscaleExec("\n\n", nil)
	old := tailscaleExecFn
	tailscaleExecFn = fn
	defer func() { tailscaleExecFn = old }()

	if _, err := tailscaleIP4(context.Background()); err == nil {
		t.Fatal("expected error when tailscale reports no IPv4 address")
	}
}

func TestTailscaleReady_UpThenIP(t *testing.T) {
	fn, calls := fakeTailscaleExec("100.64.0.9", nil)
	old := tailscaleExecFn
	tailscaleExecFn = fn
	defer func() { tailscaleExecFn = old }()

	var buf bytes.Buffer
	advertise, err := tailscaleReady(context.Background(), &buf, "tskey-z", "node-9")
	if err != nil {
		t.Fatalf("tailscaleReady: %v", err)
	}
	if advertise != "100.64.0.9" {
		t.Errorf("tailscaleReady advertise = %q, want 100.64.0.9", advertise)
	}
	if len(*calls) != 2 {
		t.Errorf("expected 2 tailscale calls (up + ip), got %v", *calls)
	}
	if !strings.Contains(buf.String(), "✔ tailnet address 100.64.0.9") {
		t.Errorf("missing tailnet address line: %q", buf.String())
	}
}

func TestTailscaleAuthKeyFromEnv(t *testing.T) {
	t.Setenv("PMCLUSTER_TAILSCALE_AUTH_KEY", "tskey-env-42")
	if got := tailscaleAuthKeyFromEnv(); got != "tskey-env-42" {
		t.Errorf("tailscaleAuthKeyFromEnv = %q, want tskey-env-42", got)
	}
}

// The join / cluster-up flag surfaces must exist so operators can opt in.
func TestTailscaleFlagsRegistered(t *testing.T) {
	if joinCmd.Flags().Lookup("tailscale") == nil {
		t.Error("joinCmd missing --tailscale flag")
	}
	if joinCmd.Flags().Lookup("tailscale-auth-key") == nil {
		t.Error("joinCmd missing --tailscale-auth-key flag")
	}
	if clusterUpCmd.Flags().Lookup("tailscale") == nil {
		t.Error("clusterUpCmd missing --tailscale flag")
	}
	if clusterUpCmd.Flags().Lookup("tailscale-auth-key") == nil {
		t.Error("clusterUpCmd missing --tailscale-auth-key flag")
	}
}
