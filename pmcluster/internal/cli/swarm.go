package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
)

// detectNodeIP returns the first non-loopback IPv4 address on this host as a
// best-effort advertise-address default for docker swarm init. Empty when no
// suitable address is found (Docker will then pick one itself).
func detectNodeIP() string {
	ifs, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifs {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				return ip4.String()
			}
		}
	}
	return ""
}

// swarmActive reports whether the local Docker daemon has an active Swarm.
func swarmActive(ctx context.Context) bool {
	dc, err := docker.New()
	if err != nil {
		return false
	}
	defer func() { _ = dc.Close() }()
	info, err := dc.Info(ctx)
	if err != nil {
		return false
	}
	return info.SwarmLocalNodeState == "active"
}

// ensureSwarmInitialized initialises the local Swarm when it is inactive —
// i.e. a first node that has never joined a Swarm. It runs `docker swarm init`
// via the docker CLI (transparent to the operator, same pattern as join).
// Nodes already in a Swarm (active/pending) and workers are left untouched.
// advertiseAddr empty → Docker auto-detects (or detectNodeIP is used).
func ensureSwarmInitialized(ctx context.Context, out io.Writer, advertiseAddr string) error {
	dc, err := docker.New()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer func() { _ = dc.Close() }()

	info, err := dc.Info(ctx)
	if err != nil {
		return fmt.Errorf("docker info: %w", err)
	}
	switch info.SwarmLocalNodeState {
	case "active", "pending", "error":
		// Already part of a Swarm (or in a transient state) — nothing to do;
		// preflight and the leader-aware daemon handle the rest.
		return nil
	}

	args := []string{"swarm", "init"}
	if advertiseAddr == "" {
		advertiseAddr = detectNodeIP()
	}
	if advertiseAddr != "" {
		args = append(args, "--advertise-addr", advertiseAddr)
	}
	jc := exec.CommandContext(ctx, "docker", args...)
	jc.Stdout = out
	jc.Stderr = out
	fmt.Fprintf(out, "→ docker %s\n", strings.Join(args, " "))
	if err := jc.Run(); err != nil {
		return fmt.Errorf("docker swarm init failed: %w", err)
	}
	fmt.Fprintln(out, "✔ Swarm initialised on this node.")
	return nil
}
