package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/storage"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// convergeStorage makes the console's storage toggle actually do something.
//
// `pmcluster storage provision` is LOCAL work — LVM, loop devices, mounts — so it has
// to run on the node itself, and the node's daemon is root there. That is the same
// rule the memory drop-in and the docker dependency follow: the setting is INTENT and
// the node makes it true. Before this, the UI's promote flipped a setting and a label,
// reported success, and left a node that could not host a single stateful stack — the
// console saying yes where the machine could only say no.
//
// It is deliberately conservative:
//   - only when this node is listed in storage_nodes;
//   - only when the data volume is not already provisioned (idempotent, silent no-op);
//   - never when the volume root already holds entries. Provision would refuse in that
//     case anyway, and calling it to watch it refuse costs a scary log line on every
//     start. A node that is already storing stacks is a node an operator has to look
//     at, not one to try again every minute.
var (
	storageDetectFn    = storage.Detect
	storageProvisionFn = storage.Provision
)

// defaultStorageSpec is the spec `storage provision` builds when every flag is left at
// its default. One definition, used by the command and by the convergence, so a
// UI-driven promotion provisions exactly what a CLI run would have provisioned.
func defaultStorageSpec() storage.Spec {
	return storage.Spec{
		Mode:       storage.ModeLoop,
		VolumeRoot: storage.DefaultVolumeRoot,
		ImagePath:  storage.DefaultImagePath,
		VG:         storage.DefaultVG,
		LV:         storage.DefaultLV,
		UnitDir:    storage.DefaultUnitDir,
	}
}

func convergeStorage(ctx context.Context, st *store.Store, hostname string, logf func(string, ...any)) {
	if st == nil || hostname == "" {
		return
	}
	nodes := stacks.ParseStorageNodes(st.SettingDefault(ctx, cluster.SettingStorageNodes(), ""))
	if !containsString(nodes, hostname) {
		return
	}

	spec := defaultStorageSpec()
	observed, err := storageDetectFn(ctx, spec)
	if err != nil {
		logf("storage: this node is a storage node but %s could not be inspected: %v", spec.VolumeRoot, err)
		return
	}
	adoptRequested := containsString(
		stacks.ParseStorageNodes(st.SettingDefault(ctx, cluster.SettingStorageAdoptNodes(), "")), hostname)

	if observed.Provisioned() {
		// A marker on a node that is already provisioned is stale — the operator asked
		// for something that is already true. Clear it, or it would sit in the settings
		// forever looking like a pending migration.
		if adoptRequested {
			clearAdoptMarker(ctx, st, hostname, logf)
		}
		// Provisioned and ours: nothing to say. A log line on every start for a
		// healthy node is how an operator learns to ignore the log.
		return
	}
	// An explicit adoption request is the ONLY way to provision around existing data.
	// Without the marker this refuses, because copying files and briefly stopping the
	// node's stacks must never be a side effect of a checkbox that says "storage".
	if adoptRequested && observed.Entries == 0 {
		// Nothing to adopt: an empty volume root just gets provisioned below.
		clearAdoptMarker(ctx, st, hostname, logf)
	}
	if adoptRequested && observed.Entries > 0 {
		adoptStorage(ctx, st, spec, hostname, logf)
		return
	}

	if observed.Entries > 0 {
		logf("storage: NOT provisioning %s — it already holds %d entries. A storage role needs an empty volume root; move the data and run `pmcluster storage provision` by hand. Stateful stacks must not be placed here until then.",
			spec.VolumeRoot, observed.Entries)
		return
	}

	logf("storage: this node is in storage_nodes but has no data volume — provisioning now (loop mode, %s)", spec.ImagePath)
	res, err := storageProvisionFn(ctx, spec)
	if err != nil {
		logf("storage: provisioning failed, so this node CANNOT host stateful stacks yet: %v", err)
		return
	}
	logf("storage: provisioned %s (%s, data %d bytes) — storage stacks can now be placed on this node",
		spec.VolumeRoot, res.Mode, res.DataBytes)
}

// adoptStorage builds the volume around data that is already on the node: drain it so
// nothing is writing, copy with verification, switch the mount, activate again.
//
// It is the same sequence `pmcluster storage migrate` documents, run by the node because
// only the node can do it. The marker is cleared afterwards: adoption is a one-shot
// request, not a mode a node sits in.
var (
	storageMigrateFn   = storage.Migrate
	nodeAvailabilityFn = dockerNodeAvailability
)

func adoptStorage(ctx context.Context, st *store.Store, spec storage.Spec, hostname string, logf func(string, ...any)) {
	logf("storage: ADOPTING %s — draining it (its stacks stop), copying %s onto a snapshot volume, then activating", hostname, spec.VolumeRoot)
	drained := true
	if err := nodeAvailabilityFn(ctx, hostname, "drain"); err != nil {
		logf("storage: cannot drain %s, so adoption is refused rather than copying under a running container: %v", hostname, err)
		return
	}
	res, err := storageMigrateFn(ctx, spec)
	if err != nil {
		logf("storage: adoption of %s FAILED, live tree untouched: %v", hostname, err)
	} else {
		logf("storage: adopted %s — %d files, %d bytes onto %s; previous tree preserved at %s",
			hostname, res.Copied, res.CopyBytes, spec.VolumeRoot, res.PreviousTree)
	}
	if err := nodeAvailabilityFn(ctx, hostname, "active"); err != nil {
		logf("storage: %s is STILL DRAINED — re-activate it by hand: docker node update --availability active %s (%v)", hostname, hostname, err)
		drained = false
	}
	if drained && err == nil {
		// Clear the marker so a later start does not adopt an already-adopted node.
		remaining := removeHostname(st.SettingDefault(ctx, cluster.SettingStorageAdoptNodes(), ""), hostname)
		if serr := st.SetSetting(ctx, cluster.SettingStorageAdoptNodes(), remaining); serr != nil {
			logf("storage: adopted %s but could not clear the adoption marker: %v", hostname, serr)
		}
	}
}

// clearAdoptMarker removes this node from the adoption list.
func clearAdoptMarker(ctx context.Context, st *store.Store, hostname string, logf func(string, ...any)) {
	remaining := removeHostname(st.SettingDefault(ctx, cluster.SettingStorageAdoptNodes(), ""), hostname)
	if err := st.SetSetting(ctx, cluster.SettingStorageAdoptNodes(), remaining); err != nil {
		logf("storage: could not clear the adoption marker for %s: %v", hostname, err)
		return
	}
	logf("storage: nothing to adopt on %s (no data to copy) — adoption request cleared", hostname)
}

// dockerNodeAvailability drains or activates a node. The adopting node is a manager, so
// it can do this to itself.
func dockerNodeAvailability(ctx context.Context, hostname, want string) error {
	out, err := exec.CommandContext(ctx, "docker", "node", "update", "--availability", want, hostname).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker node update --availability %s %s: %w (%s)", want, hostname, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// removeHostname drops one entry from a comma-separated list.
func removeHostname(list, hostname string) string {
	var kept []string
	for _, p := range strings.Split(list, ",") {
		if p = strings.TrimSpace(p); p != "" && p != hostname {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ",")
}

// containsString reports whether want is in list, trimmed.
func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
