package cli

import (
	"context"

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
	if observed.Provisioned() {
		// Provisioned and ours: nothing to say. A log line on every start for a
		// healthy node is how an operator learns to ignore the log.
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

// containsString reports whether want is in list, trimmed.
func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
