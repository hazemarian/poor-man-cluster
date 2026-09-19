package cluster

import (
	"context"
	"fmt"
	"io"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/runtime"
)

type DownInput struct {
	// Purge removes pmcluster-managed secrets, configs, and the two
	// overlay networks. SQLite state is never touched.
	Purge bool
}

type DownResult struct {
	StacksRemoved   []string
	SecretsRemoved  []string
	ConfigsRemoved  []string
	NetworksRemoved []string
}

type DownDeps struct {
	Docker   runtime.Client
	Deployer StackDeployer
	Stdout   io.Writer
}

// pmclusterManagedSecrets lists non-versioned secrets. Versioned secrets
// (cert_v*, key_v*) are discovered via label and removed separately.
var pmclusterManagedSecrets = []string{
	"admin_credentials",
	"zo_root_user_password",
}

var pmclusterManagedNetworks = []string{
	"traefik-net",
	"monitoring-net",
}

// Down is idempotent — missing resources are no-ops.
func Down(ctx context.Context, deps DownDeps, in DownInput) (*DownResult, error) {
	out := io.Discard
	if deps.Stdout != nil {
		out = deps.Stdout
	}
	res := &DownResult{}
	step := func(label string) { fmt.Fprintf(out, "▶ %s\n", label) }

	step("Removing stacks (infra, edge, observability, backup)")
	for _, s := range []string{"infra", "edge", "observability", "backup"} {
		if err := deps.Deployer.RemoveStack(ctx, s); err != nil {
			fmt.Fprintf(out, "  ⚠ stack rm %s: %v\n", s, err)
			continue
		}
		res.StacksRemoved = append(res.StacksRemoved, s)
	}

	if !in.Purge {
		step("Cluster down complete (secrets/configs/networks preserved — pass --purge to wipe)")
		return res, nil
	}

	fmt.Fprintln(out, "  Waiting briefly for stack teardown to settle…")
	waitTeardownSettle(ctx)

	step("Purging pmcluster-managed Swarm secrets")
	for _, name := range pmclusterManagedSecrets {
		if err := deps.Docker.SecretRemove(ctx, name); err != nil {
			fmt.Fprintf(out, "  ⚠ %s: %v\n", name, err)
			continue
		}
		res.SecretsRemoved = append(res.SecretsRemoved, name)
	}

	allSecrets, err := deps.Docker.SecretList(ctx, pmclusterLabel, "true")
	if err != nil {
		fmt.Fprintf(out, "  ⚠ secret list: %v\n", err)
	} else {
		for _, name := range allSecrets {
			if rmErr := deps.Docker.SecretRemove(ctx, name); rmErr != nil {
				fmt.Fprintf(out, "  ⚠ %s: %v\n", name, rmErr)
				continue
			}
			res.SecretsRemoved = append(res.SecretsRemoved, name)
		}
	}

	step("Purging pmcluster-managed Docker configs")
	allConfigs, err := deps.Docker.ConfigList(ctx, pmclusterLabel, "true")
	if err != nil {
		fmt.Fprintf(out, "  ⚠ config list: %v\n", err)
	} else {
		for _, name := range allConfigs {
			if rmErr := deps.Docker.ConfigRemove(ctx, name); rmErr != nil {
				fmt.Fprintf(out, "  ⚠ %s: %v\n", name, rmErr)
				continue
			}
			res.ConfigsRemoved = append(res.ConfigsRemoved, name)
		}
	}

	step("Purging pmcluster-managed overlay networks")
	for _, name := range pmclusterManagedNetworks {
		if err := deps.Docker.NetworkRemove(ctx, name); err != nil {
			fmt.Fprintf(out, "  ⚠ %s: %v\n", name, err)
			continue
		}
		res.NetworksRemoved = append(res.NetworksRemoved, name)
	}

	step("Purge complete (SQLite at ~/.pmcluster preserved — delete manually if desired)")
	return res, nil
}

func waitTeardownSettle(ctx context.Context) {
	const settleSeconds = 5
	timer := newTimer(settleSeconds)
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}
