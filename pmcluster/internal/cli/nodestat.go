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
	"encoding/json"
	"os"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodereport"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runners"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodestat"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

const (
	// nodeStatSampleInterval paces the /proc sample. 15s is frequent enough
	// that the panel is current when an operator opens it and cheap enough that
	// the sample never shows up in a load average.
	nodeStatSampleInterval = 15 * time.Second
	// nodeStatTailInterval paces the leader's read of the collector's file.
	// Independent of the sample interval on purpose: a slow sample must not
	// delay a read, and a read must never trigger a sample.

	// nodeStatPathEnv overrides the path for tests and for a host whose
	// /var/stack lives elsewhere.
)

// startNodeStatSampler runs the /proc sampler for this node until ctx ends.
//
// It is started on EVERY node (not just the leader): the whole point is that each node
// describes itself. It no longer waits for a collector endpoint — after the collector
// became leader-only, a worker has nothing local to export to, and waiting for one is
// exactly how four nodes went missing from the panel. The sample is POSTed to the leader
// on the swarm mTLS channel instead; the OTLP publish is still attempted and is a no-op
// where there is no exporter.
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
	nodeStatTickWith(ctx, dc, s, log, nodeReportSender(dc))
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
	// No direct Publish: see above. A sample that cannot be delivered is simply logged
	// (one debug line, next tick retries) rather than exported twice on the leader.
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

// nodeReportSender builds the client a node uses to report itself to the leader. It is
// the same transport the runner assignment uses: swarm mTLS, manager addresses from the
// live swarm, no published port and no shared secret.
func nodeReportSender(dc runtime.Client) *runners.Client {
	if dc == nil {
		return nil
	}
	return &runners.Client{Managers: func(ctx context.Context) ([]string, error) {
		return managerHosts(ctx, dc)
	}}
}

// nodeStatTickWith is nodeStatTick with the sender injected, so a test can drive one tick
// without a leader.
// reportSender is the one method the tick needs. An interface, so a test can drive one
// tick and assert what would be sent without a leader to send it to.
type reportSender interface {
	PostJSON(ctx context.Context, path string, body []byte) error
}

func nodeStatTickWith(ctx context.Context, dc runtime.Client, s *nodestat.Sampler, log zerolog.Logger, sender reportSender) {
	smp, err := s.Collect(time.Now())
	if err != nil {
		log.Warn().Err(err).Msg("node stat sample incomplete")
	}
	if dc != nil {
		if _, err := dc.Info(ctx); err != nil {
			smp.DockerUp = 0
		}
	}
	// EVERY node reports, the leader included, and the leader's report handler is the
	// single place that emits metrics (it is the only node with a collector). Publishing
	// here as well would double every point on the leader's own series.
	if sender != nil {
		body, merr := json.Marshal(smp)
		if merr != nil {
			log.Warn().Err(merr).Msg("node report: encode")
		} else if perr := sender.PostJSON(ctx, nodereport.Path, body); perr != nil {
			log.Debug().Err(perr).Msg("node report: not delivered to the leader")
		}
	}
	// No direct Publish: see above. A sample that cannot be delivered is simply logged
	// (one debug line, next tick retries) rather than exported twice on the leader.
}
