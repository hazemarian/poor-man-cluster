package doctor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// checkBackupAgents answers the question the coverage check cannot: does every storage node
// actually RUN the backup agent?
//
// Coverage is structural — where a stack's data lives and whether that node is in the storage
// set. It says nothing about whether the agent is installed on that node, and the agent is
// OPT-IN per node (`pmcluster backup-agent install`). Measured on production 2026-11-16: one of
// three storage nodes had no unit, no timer, no policy and no env at all — its share of the data
// (a live stack, five sqlite databases) was never archived — while `doctor` reported
// `✓ backup coverage 15 stack(s) hold a volume, all on a storage node` on every run.
//
// The signal is "has this node EVER reported a run". Every agent posts a record to a manager
// when it ships, and the artifact name carries the node id it was taken on
// (backup-<node>-<ts>-stack-<stack>.tar.gz), so a node that has never produced a row is a node
// with no working agent. A WINDOW of days rather than "the newest row is fresh" is deliberate:
// the run table is known to be an unreliable clock (rows stuck for days while artifacts were
// perfectly fresh), and this check must not inherit that — it only has to catch "never".
// backupView is what the backup checks need, so the leader view (Input) and the node-local
// view (NodeInput) can both run them. A follower manager mirrors the settings, so it can answer
// "is every storage node reporting" too — and that is exactly where an operator is standing when
// a node has no agent.
type backupView struct {
	Store      *store.Store
	Runtime    runtime.Client
	VolumeRoot string
	Now        time.Time
	// RunsAvailable says this process can see the backup RUN HISTORY. Only the leader can: the
	// backups table is node-local and NOT mirrored (ADR 0005's exclusion list), so a follower's
	// store answers "no runs ever" for a cluster that is backing up hourly. Measured on the fleet
	// the moment the check reached a follower: it called all three storage nodes silent.
	RunsAvailable bool
}

func (in Input) backupView() backupView {
	return backupView{Store: in.Store, Runtime: in.Docker, VolumeRoot: in.VolumeRoot, Now: in.Now, RunsAvailable: true}
}

func (in NodeInput) backupView() backupView {
	// A follower cannot answer this one, and must not pretend to: see RunsAvailable.
	return backupView{Store: in.Store, Runtime: in.Runtime, VolumeRoot: in.VolumeRoot}
}

func checkBackupAgents(ctx context.Context, v backupView) Check {
	const name = "backup agents"
	if v.Store == nil || v.Runtime == nil {
		return Check{Name: name, Status: StatusSkip, Detail: "no store or swarm client on this host"}
	}
	if !v.RunsAvailable {
		return Check{Name: name, Status: StatusSkip,
			Detail: "the backup run history is not mirrored (the backups table is node-local), so this can only be answered on the leader — run `pmcluster doctor` there"}
	}
	raw := v.Store.SettingDefault(ctx, cluster.SettingStorageNodes(), "")
	configured := stacks.ParseStorageNodes(raw)
	if len(configured) == 0 {
		// No designation at all: `checkBackupCoverage` already reports that, and a node cannot
		// be blamed for not running the agent for a role nobody assigned it.
		return Check{Name: name, Status: StatusSkip, Detail: "no storage nodes are designated (see backup coverage)"}
	}

	nodes, err := v.Runtime.NodeList(ctx)
	if err != nil {
		return Check{Name: name, Status: StatusSkip, Detail: "could not list swarm nodes: " + err.Error()}
	}
	byHost := map[string]string{} // hostname -> node id
	for _, n := range nodes {
		byHost[n.Hostname] = n.ID
	}

	window := 7 * 24 * time.Hour
	cutoff := v.now().Add(-window).Unix()
	rows, err := v.Store.ListBackups(ctx, backupAgentScanLimit)
	if err != nil {
		return Check{Name: name, Status: StatusSkip, Detail: "could not list backup runs: " + err.Error()}
	}
	// Which node ids are evidenced in the window, from the artifact names.
	evidenced := map[string]bool{}
	for _, r := range rows {
		if r.StartedAt < cutoff {
			continue
		}
		if id := nodeFromArchivePaths(r.ArchivePaths); id != "" {
			evidenced[id] = true
		}
	}

	var silent []string
	for _, host := range configured {
		id, known := byHost[host]
		if !known {
			// A hostname in the setting that no swarm node answers to is a different defect
			// (`checkPlacementPins` and the storage reconcile own it); not this check's.
			continue
		}
		if !evidenced[id] {
			silent = append(silent, host)
		}
	}
	if len(silent) > 0 {
		return Check{Name: name, Status: StatusFail,
			Detail: fmt.Sprintf("no backup run in the last %d days was recorded from: %s — the agent is opt-in per node, so a storage node that is never installed archives nothing while its data sits on the volume. Install it with `pmcluster backup-agent install` on each of them, then confirm with `pmcluster backup-agent run --force`.",
				int(window.Hours()/24), strings.Join(silent, ", "))}
	}
	return Check{Name: name, Status: StatusOK,
		Detail: fmt.Sprintf("%d storage node(s) reported a backup run in the last %d days", len(configured), int(window.Hours()/24))}
}

// backupAgentScanLimit bounds how many run rows the agent check reads. A week of hourly runs
// across a dozen stacks is a few thousand rows; the listing returns newest first, so a limit is
// enough to cover the window without walking the whole table.
const backupAgentScanLimit = 5000

// nodeFromArchivePaths extracts the node id from recorded artifact paths
// (backup-<node>-<ts>-stack-<stack>.tar.gz under the archive root).
func nodeFromArchivePaths(paths string) string {
	for _, p := range strings.FieldsFunc(paths, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		base := p
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		if !strings.HasPrefix(base, "backup-") {
			continue
		}
		rest := strings.TrimPrefix(base, "backup-")
		if i := strings.Index(rest, "-"); i > 0 {
			return rest[:i]
		}
	}
	return ""
}

// now is the clock these checks compare against, overridable in tests.
func (v backupView) now() time.Time {
	if v.Now.IsZero() {
		return time.Now()
	}
	return v.Now
}
