package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// --- Tailscale helpers --------------------------------------------------------
//
// pmcluster never requires a tailnet. These helpers are opt-in: when the
// operator passes --tailscale on `join` or `cluster up`/`setup`, the node is
// brought onto a private WireGuard-based tailnet first and the Swarm join/init
// advertises the tailnet address instead of a public IP — so swarm traffic
// (2377/7946/4789 + any storage ports) needs no firewall rules between nodes.
//
// Everything here shells out to the `tailscale` CLI (the official client).
// Overridable exec seams (tailscaleExecFn) keep the tests hermetic.

// tailscaleExecFn runs `tailscale <args...>` and returns combined output.
// Overridable so tests never need a real tailnet.
var tailscaleExecFn = func(ctx context.Context, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, "tailscale", args...)
	return c.CombinedOutput()
}

// tailscaleTimeout bounds `tailscale up` — a hung login must not stall the
// join forever.
const tailscaleTimeout = 60 * time.Second

// tailscaleUp brings this node onto the tailnet. authKey may be empty when the
// node is already logged in (tailscale up works without one in that case);
// hostname sets the tailnet node name when non-empty.
func tailscaleUp(ctx context.Context, out io.Writer, authKey, hostname string) error {
	args := []string{"up"}
	if authKey != "" {
		args = append(args, "--auth-key="+authKey)
	}
	if hostname != "" {
		args = append(args, "--hostname="+hostname)
	}
	fmt.Fprintf(out, "→ tailscale %s\n", strings.Join(args, " "))
	ctx, cancel := context.WithTimeout(ctx, tailscaleTimeout)
	defer cancel()
	outB, err := tailscaleExecFn(ctx, args...)
	if err != nil {
		return fmt.Errorf("tailscale up failed: %w: %s", err, strings.TrimSpace(string(outB)))
	}
	fmt.Fprintln(out, "✔ node joined the tailnet.")
	return nil
}

// tailscaleIP4 returns this node's tailnet IPv4 (first address from
// `tailscale ip -4`), or an error when the node is not on a tailnet.
func tailscaleIP4(ctx context.Context) (string, error) {
	outB, err := tailscaleExecFn(ctx, "ip", "-4")
	if err != nil {
		return "", fmt.Errorf("tailscale ip -4 failed: %w", err)
	}
	line := strings.TrimSpace(string(outB))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		return "", errors.New("tailscale ip -4 returned no address — this node may not be on a tailnet")
	}
	return line, nil
}

// tailscaleReady brings the node onto the tailnet (if requested) and returns
// the tailnet IPv4 to advertise to the Swarm. Called from the join + first-node
// flows when --tailscale is set. Failing loudly is correct here: the operator
// explicitly asked for a tailnet, so silently falling back to a public IP would
// defeat the purpose.
func tailscaleReady(ctx context.Context, out io.Writer, authKey, hostname string) (string, error) {
	if err := tailscaleUp(ctx, out, authKey, hostname); err != nil {
		return "", err
	}
	ip, err := tailscaleIP4(ctx)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(out, "✔ tailnet address %s\n", ip)
	return ip, nil
}

// tailscaleAuthKeyFromEnv mirrors the PMCLUSTER_* config convention so CI /
// scripted joins can pass the key without it landing in shell history.
func tailscaleAuthKeyFromEnv() string {
	return os.Getenv("PMCLUSTER_TAILSCALE_AUTH_KEY")
}
