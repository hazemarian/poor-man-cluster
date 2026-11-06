package backupagent

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// LiveMtimeWindow is how recent a change has to be for a directory nothing
// mounts to still count as live.
//
// It exists because "no container mounts this" is not the same as "this is
// dead": a stack deployed between runs may not be up yet, a stack scaled to zero
// a minute ago has no container, and a restore drains the stack it is restoring.
// A day is longer than the hourly interval, so anything genuinely in use is seen
// live at least once before its directory can be called an orphan.
const LiveMtimeWindow = 26 * time.Hour

// PlanFromMounts derives the plan from what THIS NODE's containers actually use.
//
// It is the answer for a node without swarm access (a worker): the local docker
// daemon knows which stacks have containers here and which host directories those
// containers mount, and answering needs no swarm API, no cross-node call and no
// credential — the three things a worker does not have.
//
// A directory is live when any of these holds:
//
//   - a container on this node bind-mounts it (running OR stopped — a crashed
//     stack whose container is still on the node is not a leftover);
//   - something under it changed within LiveMtimeWindow (deployed or scaled to
//     zero since the last run);
//   - an artifact for that stack was written within LiveMtimeWindow (we were
//     backing it up until recently).
//
// Everything else is an orphan: named, reported, not archived.
func PlanFromMounts(root string, mounts []runtime.HostMount, recentlyArchived map[string]bool, now time.Time) (Plan, error) {
	plan, err := PlanFromDisk(root)
	if err != nil {
		return plan, err
	}
	plan.GeneratedAt = now.UTC().Unix()
	plan.VolumeRoot = root

	mounted := make(map[string]bool, len(mounts))
	cleanRoot := filepath.Clean(root)
	for _, m := range mounts {
		if m.Source == "" {
			continue
		}
		// Which top-level directory of the volume root does this mount belong
		// to? A mount of /var/stack/data/shop/data belongs to "shop"; a mount
		// somewhere else on the host says nothing about this tree.
		rel, rerr := filepath.Rel(cleanRoot, filepath.Clean(m.Source))
		if rerr != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
		if top != "" {
			mounted[top] = true
		}
	}

	cutoff := now.Add(-LiveMtimeWindow)
	kept := plan.Stacks[:0]
	for _, s := range plan.Stacks {
		live := mounted[s.Name] ||
			recentlyArchived[s.Name] ||
			NewestMtime(filepath.Join(root, s.Name)) >= cutoff.Unix()
		if live {
			kept = append(kept, s)
			continue
		}
		plan.Orphans = append(plan.Orphans, s.Name)
	}
	plan.Stacks = kept
	sort.Strings(plan.Orphans)
	return plan, nil
}
