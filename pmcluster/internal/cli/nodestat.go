package cli

// Per-node resource visibility, daemon side.
//
// Two halves, both driven from serve:
//
//  1. EVERY daemon samples its own /proc every 15s and publishes the sample as
//     OTLP metrics (internal/nodestat.Publish). The collector receives it over
//     the ingress-published 4318 and appends it to the node-stats JSONL.
//  2. The LEADER's daemon tails that JSONL and upserts one row per node. Only
//     the leader: it is the node that runs the collector task, so it is the only
//     node where the file exists, and it is the only node whose store the
//     console reads.
//
// The split is what makes the panel survive a telemetry outage: the reader
// needs a file on a local disk, not a working Parseable.

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodestat"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

const (
	// nodeStatSampleInterval paces the /proc sample. 15s is frequent enough
	// that the panel is current when an operator opens it and cheap enough that
	// the sample never shows up in a load average.
	nodeStatSampleInterval = 15 * time.Second
	// nodeStatTailInterval paces the leader's read of the collector's file.
	// Independent of the sample interval on purpose: a slow sample must not
	// delay a read, and a read must never trigger a sample.
	nodeStatTailInterval = 15 * time.Second

	// nodeStatDefaultPath is where the collector's file exporter writes, and
	// where the stack bind-mounts it. Kept as a constant (not derived from the
	// volume root) because the bind is a fixed host path in the stack file.
	nodeStatDefaultPath = "/var/stack/nodestat/nodestat.jsonl"

	// nodeStatPathEnv overrides the path for tests and for a host whose
	// /var/stack lives elsewhere.
	nodeStatPathEnv = "PMCLUSTER_NODESTAT_PATH"
)

// nodeStatFilePath returns the collector's JSONL path.
func nodeStatFilePath() string {
	if p := strings.TrimSpace(os.Getenv(nodeStatPathEnv)); p != "" {
		return p
	}
	return nodeStatDefaultPath
}

// startNodeStatSampler runs the /proc sampler for this node until ctx ends.
//
// It is started on EVERY node (not just the leader): the whole point is that
// each node describes itself. It is started only when a collector endpoint
// resolved — publishing into the void would be pure waste, and the daemon
// already logs once why telemetry is off.
func startNodeStatSampler(ctx context.Context, dc runtime.Client, log zerolog.Logger) {
	go func() {
		host := nodeStatHostname(ctx, dc, log)
		role := localRole(ctx, dc)
		s := nodestat.NewSampler("/", host, role)
		t := time.NewTicker(nodeStatSampleInterval)
		defer t.Stop()
		for {
			nodeStatTick(ctx, dc, s, log)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// nodeStatTick is one sample → export cycle, split out of the loop so a test can
// drive exactly one tick. It has NO leadership condition: the collector's
// /var/stack/nodestat file is written by a pipeline that ends in a `file`
// exporter, so there is no loop for the leader to avoid by staying silent.
// Suppressing the leader's own sample was the bug that left the leader without
// a row in the Node resources panel.
func nodeStatTick(ctx context.Context, dc runtime.Client, s *nodestat.Sampler, log zerolog.Logger) {
	smp, err := s.Collect(time.Now())
	if err != nil {
		// A partial /proc (a racing exit, a restricted mount) must not stop
		// the loop: the next tick usually reads everything.
		log.Warn().Err(err).Msg("node stat sample incomplete")
	}
	// The engine socket is the fourth tint rule: a node whose Docker does not
	// answer cannot start or stop anything, however healthy its kernel looks.
	if dc != nil {
		if _, err := dc.Info(ctx); err != nil {
			smp.DockerUp = 0
		}
	}
	nodestat.Publish(ctx, smp)
}

// localHostname prefers the swarm's name for this node over a short hostname:
// the panel joins these rows against `docker node ls`, and a FQDN/short-name
// mismatch would show an empty process column for the very node in trouble.
func nodeStatHostname(ctx context.Context, dc runtime.Client, log zerolog.Logger) string {
	if dc != nil {
		if info, err := dc.Info(ctx); err == nil && info.Name != "" {
			return info.Name
		}
	}
	h, err := os.Hostname()
	if err != nil {
		log.Warn().Err(err).Msg("hostname unavailable for node stats")
		return "unknown"
	}
	return h
}

// localRole names this node's swarm role for the panel's role column. Read once
// at startup: a role change restarts the swarm membership anyway.
func localRole(ctx context.Context, dc runtime.Client) string {
	if dc == nil {
		return ""
	}
	host := nodeStatHostname(ctx, dc, zerolog.Nop())
	nodes, err := dc.NodeList(ctx)
	if err != nil {
		return ""
	}
	for _, n := range nodes {
		if n.Hostname == host {
			return n.Role
		}
	}
	return ""
}

// runNodeStatTailer reads the collector's file and keeps node_stats current.
//
// It is leader-only: the collector is pinned to node.labels.pmcluster.leader by a
// hard constraint, so the file exists on the leader's disk and only the leader's
// daemon is the one the console talks to.
func runNodeStatTailer(ctx context.Context, st *store.Store, dc runtime.Client, log zerolog.Logger) {
	if st == nil {
		return
	}
	path := nodeStatFilePath()
	r := nodestat.NewReader(path)
	log.Info().Str("path", path).Msg("node stats: tailing the collector's file")

	t := time.NewTicker(nodeStatTailInterval)
	defer t.Stop()
	missingLogged := false
	for {
		if res := tailNodeStatsOnce(ctx, r, st, dc, log); res.Missing && !missingLogged {
			log.Info().Str("path", path).Msg("node stats: file not written yet (collector deploying?)")
			missingLogged = true
		} else if !res.Missing {
			missingLogged = false
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// tailNodeStatsOnce is one read → upsert → prune cycle. Split out of the loop
// so the whole path is exercised by tests without waiting for a ticker.
func tailNodeStatsOnce(ctx context.Context, r *nodestat.Reader, st *store.Store, dc runtime.Client, log zerolog.Logger) nodestat.ReadResult {
	res, err := r.Read()
	if err != nil {
		log.Warn().Err(err).Msg("node stats: read failed")
		return res
	}
	if res.Missing {
		return res
	}
	if res.Malformed > 0 {
		log.Warn().Int("skipped", res.Malformed).Int("total", r.MalformedLines()).
			Msg("node stats: skipped unparseable lines in the collector output")
	}
	if len(res.Stats) > 0 {
		if err := st.UpsertNodeStats(ctx, res.Stats, time.Now().Unix()); err != nil {
			log.Warn().Err(err).Msg("node stats: upsert failed")
		}
	}
	pruneNodeStats(ctx, st, dc, log)
	return res
}

// pruneNodeStats deletes rows for nodes no longer in the swarm.
//
// A node list failure SKIPS the prune instead of running it with an empty list:
// a transient manager outage must never wipe the panel's rows. The reader's own
// per-node memory is untouched here — a node that merely went quiet keeps its
// last row, which is the difference between "stopped reporting" (grey, aged)
// and "left the swarm" (gone).
func pruneNodeStats(ctx context.Context, st *store.Store, dc runtime.Client, log zerolog.Logger) {
	if dc == nil {
		return
	}
	nodes, err := dc.NodeList(ctx)
	if err != nil {
		return
	}
	keep := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.Hostname != "" {
			keep = append(keep, n.Hostname)
		}
	}
	if len(keep) == 0 {
		return
	}
	if n, err := st.DeleteNodeStatsExcept(ctx, keep); err != nil {
		log.Warn().Err(err).Msg("node stats: prune failed")
	} else if n > 0 {
		log.Info().Int64("rows", n).Msg("node stats: pruned rows for nodes no longer in the swarm")
	}
}

// syncLeaderLabel makes pmcluster.leader authoritative on THIS node — stamped
// ("true") when this node is leader, cleared ("") when it is not — and reports
// whether it CHANGED anything.
//
// It is called on every leadership transition AND on every leader tick
// (keepLeaderLabel below), and it also clears the label from any OTHER node that
// still carries it when this node is leader. Both halves matter now that the
// collector's placement is a hard CONSTRAINT on this label:
//
//   - exactly one node must match, or the scheduler may place the collector on a
//     stale former leader whose disk nobody reads (the label outliving a failed
//     clear is the same failure as never setting it);
//   - the label must exist, because a constraint has no schedulable floor to fall
//     back to. A single write per election would leave the collector at 0/1 after
//     one Docker API hiccup until the next election.
//
// Best-effort per write: one failure is logged and the tick retries, since
// refusing to serve over a label is worse than a collector that is briefly
// unschedulable. The return value exists so the caller forces exactly ONE service
// restart per real transition instead of on every tick.
func syncLeaderLabel(ctx context.Context, dc runtime.Client, leader bool, log zerolog.Logger) bool {
	if dc == nil {
		return false
	}
	host := nodeStatHostname(ctx, dc, log)
	nodes, err := dc.NodeList(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("leadership label: cannot list nodes")
		return false
	}
	want := ""
	if leader {
		want = "true"
	}
	changed := false
	for _, n := range nodes {
		if n.Hostname == host {
			if cur, ok := n.Labels[runtime.LeaderNodeLabel]; ok && cur == want {
				continue // already correct: no API write on every tick
			}
			if err := dc.SetNodeLabel(ctx, n.ID, runtime.LeaderNodeLabel, want); err != nil {
				log.Warn().Err(err).Str("node", host).Bool("leader", leader).
					Msg("leadership label: update failed")
				continue
			}
			log.Info().Str("node", host).Bool("leader", leader).
				Str("label", runtime.LeaderNodeLabel).Msg("leadership label updated")
			changed = true
			continue
		}
		// Not this node. While this node is leader, a label left on another node
		// is stale by definition (a clear that never landed, or a node that left
		// and rejoined) and would give the collector's constraint a second match.
		if leader && n.Labels[runtime.LeaderNodeLabel] == "true" {
			if err := dc.SetNodeLabel(ctx, n.ID, runtime.LeaderNodeLabel, ""); err != nil {
				log.Warn().Err(err).Str("node", n.Hostname).
					Msg("leadership label: cannot clear a stale label")
				continue
			}
			log.Info().Str("node", n.Hostname).Str("label", runtime.LeaderNodeLabel).
				Msg("leadership label cleared from a node that is not the leader")
			changed = true
		}
	}
	return changed
}

// leaderLabelReassertInterval paces keepLeaderLabel. A var so tests can drive the
// loop without waiting half a minute.
var leaderLabelReassertInterval = 30 * time.Second

// keepLeaderLabel re-asserts pmcluster.leader=true for as long as this node holds
// leadership, and stops when the caller's context ends (leadership lost, daemon
// stopping). The label is a precondition of the collector's placement, so it is
// maintained like one: a missed write is repaired on the next tick instead of
// waiting for an election that may never come.
func keepLeaderLabel(ctx context.Context, dc runtime.Client, log zerolog.Logger) {
	if dc == nil {
		return
	}
	t := time.NewTicker(leaderLabelReassertInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			syncLeaderLabel(ctx, dc, true, log)
		}
	}
}

// nodeStatCollectorService is the collector's Swarm service name
// (`<stack>_<service>`). It is a constant here because the restart below is the
// one place the daemon names a platform service directly.
const nodeStatCollectorService = "observability_otel-collector"

// replaceNodeStatCollector forces the collector task to be scheduled again after
// the leadership label moved.
//
// Placement is only evaluated when a task is CREATED: a running task stays where
// it is until the service is updated or restarted, even when the constraint that
// selected its node no longer matches. So a leadership change alone would leave
// the collector on the old leader while the node-stats file it writes sits on a
// node whose daemon no longer reads it. One force-update per real label change
// closes that gap; it is best-effort because on a first boot the stack may not be
// deployed yet (the reconcile pass deploys it with the label already in place).
func replaceNodeStatCollector(ctx context.Context, dc runtime.Client, log zerolog.Logger) {
	if dc == nil {
		return
	}
	if err := dc.ServiceRestart(ctx, nodeStatCollectorService); err != nil {
		log.Debug().Err(err).Str("service", nodeStatCollectorService).
			Msg("collector re-placement skipped (service not deployed yet?)")
		return
	}
	log.Info().Str("service", nodeStatCollectorService).
		Msg("collector re-placed on the new leader")
}
