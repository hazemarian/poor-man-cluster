package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backupagent"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
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
func publishBackupPlan(ctx context.Context, dc runtime.Client, log func(format string, args ...any)) {
	if dc == nil {
		if log != nil {
			log("backup plan not published: no docker client, so the agent keeps the volume-root fallback")
		}
		return
	}
	tick := time.NewTicker(planPublishInterval)
	defer tick.Stop()
	for {
		if err := writeBackupPlanOnce(ctx, dc); err != nil && log != nil {
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
func writeBackupPlanOnce(ctx context.Context, dc runtime.Client) error {
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
	// The volume root comes from the policy file the installer wrote (it carries
	// the same setting the daemon would read, resolved at install time), so this
	// loop does not need the store — which is what lets it run before the
	// leadership wait, when no store is open yet.
	root := agentVolumeRootFromPolicy()
	plan, err := backupagent.PlanFromStacks(root, deployed)
	if err != nil {
		return err
	}
	plan.VolumeRoot = root
	return backupagent.WritePlan(filepath.Join(defaultStateDir, planCacheName), plan)
}

// agentVolumeRootFromPolicy reads the volume root the backup agent was installed
// with. Anything unreadable falls back to the default: a wrong root here would
// publish a plan of directories that do not exist, and the agent's own
// verification (it proves the root is its volume) is the real guard.
func agentVolumeRootFromPolicy() string {
	if v := os.Getenv("PMCLUSTER_VOLUME_ROOT"); v != "" {
		return v
	}
	root := defaultVolumeRoot
	b, err := os.ReadFile(backupPolicyFile)
	if err != nil {
		return root
	}
	var pol struct {
		VolumeRoot string `json:"volume_root"`
	}
	if json.Unmarshal(b, &pol) == nil && pol.VolumeRoot != "" {
		return pol.VolumeRoot
	}
	return root
}
