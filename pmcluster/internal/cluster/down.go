package cluster

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// DownInput controls a cluster teardown.
type DownInput struct {
	// Purge removes pmcluster-managed secrets, configs, and the two
	// overlay networks. When StoreDir is also set, the local SQLite store
	// is backed up to a restorable tarball and then deleted.
	Purge bool

	// StoreDir is the local store directory (the config DataDir). When Purge
	// is set and StoreDir is non-empty, Down writes a restorable safety backup
	// (data.db + .encryption_key + config.yaml) to
	// <StoreDir>/purge-backup-<RFC3339>.tar.gz, records its path in
	// DownResult.StoreBackupPath, then deletes the store files. The CLI layer
	// supplies this (the cluster package must not import config/CLI).
	StoreDir string
}

// DownResult reports what a teardown actually removed.
type DownResult struct {
	StacksRemoved   []string
	SecretsRemoved  []string
	ConfigsRemoved  []string
	NetworksRemoved []string

	// StoreBackupPath is the path of the restorable store backup written when
	// Purge was combined with a StoreDir. Empty when no backup was written.
	StoreBackupPath string
}

// DownDeps are the collaborators Down needs: runtime client, deployer and
// output sink.
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

	step("Removing stacks (infra, edge, observability, backup, sso)")
	for _, s := range []string{"infra", "edge", "observability", "backup", "sso"} {
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
		// Overlay network removal is asynchronous in the swarm store: the
		// remove call returns while the network can still be inspected in a
		// "removing" state. If a subsequent `cluster up` runs before the
		// removal completes, its existence check sees the stale network,
		// skips creation, and the later stack deploy fails with "network X
		// not found". Wait for the network to be fully gone so the next up
		// deterministically recreates it.
		if err := waitNetworkGone(ctx, deps.Docker, name, 15*time.Second); err != nil {
			fmt.Fprintf(out, "  ⚠ %s: network still present after removal (%v)\n", name, err)
		}
	}

	// NEW: `--purge` now means "delete EVERYTHING". Back up the local store to
	// a restorable tarball (data.db + .encryption_key + config.yaml) so an
	// accidental wipe can be undone with `cluster reset --restore`, then delete
	// the store files themselves. The backup archive is deliberately KEPT.
	if in.StoreDir != "" {
		// A purge on a box with no local store (never initialised) has nothing
		// to back up — skip rather than fail the whole teardown.
		if _, statErr := os.Stat(filepath.Join(in.StoreDir, "data.db")); os.IsNotExist(statErr) {
			fmt.Fprintln(out, "  (no local store found to back up — skipping store deletion)")
		} else {
			step("Backing up the local store before deleting it")
			backupPath, err := WriteStoreBackup(in.StoreDir)
			if err != nil {
				// A failed backup must never silently proceed to delete the store.
				return res, fmt.Errorf("write store backup: %w", err)
			}
			res.StoreBackupPath = backupPath
			fmt.Fprintf(out, "  ✓ store backed up to %s\n", backupPath)

			step("Deleting the local store (data.db, .encryption_key, config.yaml)")
			if err := deleteStoreFiles(in.StoreDir); err != nil {
				return res, fmt.Errorf("delete local store: %w", err)
			}
			fmt.Fprintln(out, "  ✓ local store deleted (backup archive kept)")
		}
	}

	step("Purge complete")
	if res.StoreBackupPath != "" {
		fmt.Fprintf(out, "  Restore with: pmcluster cluster reset --restore %s\n", res.StoreBackupPath)
	}
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

// waitNetworkGone polls the runtime client until the named overlay network
// can no longer be inspected, or the deadline expires. Overlay network
// removal in Swarm is asynchronous: NetworkRemove returns before the
// network object leaves the store, so without this wait a fast-following
// cluster up would see the stale network and skip recreating it.
func waitNetworkGone(ctx context.Context, d runtime.Client, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		exists, err := d.NetworkExists(ctx, name)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", timeout)
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
