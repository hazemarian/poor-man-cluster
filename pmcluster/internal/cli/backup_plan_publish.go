package cli

import (
	"context"
	"path/filepath"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backupagent"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// planPublishInterval is how often the daemon republishes this node's plan. It
// only has to keep up with stack create/remove, and a stale plan is caught on
// the agent side by the mtime guard, so minutes are plenty.
const planPublishInterval = 15 * time.Minute

// publishBackupPlan keeps this node's backup plan in step with the swarm.
//
// The agent archives one artifact per directory under the volume root, because a
// directory cannot know whether its stack still exists. The daemon can: it lists
// services. Publishing the plan turns "archive every directory forever" into
// "archive the stacks that exist, and name the leftovers", which on one production
// node was 87 MB of an hourly payload for volumes of deleted stacks.
//
// A node that cannot list services (a worker, which by role holds no data) simply
// keeps the disk-derived fallback — the same behaviour as before, which is why
// this is additive rather than a migration.
func publishBackupPlan(ctx context.Context, dc runtime.Client, st *store.Store, log func(format string, args ...any)) {
	if dc == nil {
		return
	}
	tick := time.NewTicker(planPublishInterval)
	defer tick.Stop()
	for {
		if err := writeBackupPlanOnce(ctx, dc, st); err != nil && log != nil {
			log("backup plan not published: %v (the agent falls back to the volume-root listing)", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// writeBackupPlanOnce builds and writes the plan from the live environment.
func writeBackupPlanOnce(ctx context.Context, dc runtime.Client, st *store.Store) error {
	svcs, err := dc.ServiceList(ctx)
	if err != nil {
		return err
	}
	deployed := make(map[string]bool, len(svcs))
	for _, s := range svcs {
		if s.Stack != "" {
			deployed[s.Stack] = true
		}
	}
	root := cluster.BackupRootDir()
	if st != nil {
		if vr := st.SettingDefault(ctx, cluster.SettingVolumeRoot(), ""); vr != "" {
			root = vr
		}
	}
	plan, err := backupagent.PlanFromStacks(root, deployed)
	if err != nil {
		return err
	}
	plan.VolumeRoot = root
	return backupagent.WritePlan(filepath.Join(defaultStateDir, planCacheName), plan)
}
