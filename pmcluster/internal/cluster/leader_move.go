package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// Leading the Swarm is decided by Raft, not by a setting: there is no
// leadership-transfer API, and `docker node promote` on an existing manager is a
// no-op. The one supported lever is membership — take the leader's manager role
// away and the remaining managers elect one of themselves — so that is what a
// leadership move does, in a bounded, verified loop instead of a fire-and-forget
// demote.
//
// This exists so failover behaviour can be tested deliberately (the kit
// adoption, the collector pin, the storage set) without taking a node down. It
// is not on any automatic path: nothing in the daemon or the reconcile loop
// calls it.

// LeaderMove is the outcome of a completed move.
type LeaderMove struct {
	// From is the leader observed before the move.
	From string
	// Leader is the leader observed afterwards — always where leadership
	// actually is, which is the target on success.
	Leader string
	// Elections is how many forced elections it took (0 when the target was
	// already leading).
	Elections int
}

// Changed reports whether leadership moved.
func (m LeaderMove) Changed() bool { return m.Elections > 0 }

// LeaderMoveRefusal is a refusal the operator can act on — the target is not a
// manager, the manager set is too small, the swarm has no quorum. The API maps
// it to 409; a Docker failure is a different error and stays a 502.
type LeaderMoveRefusal struct{ reason string }

func (e *LeaderMoveRefusal) Error() string { return e.reason }

func refuseMove(format string, args ...any) error {
	return &LeaderMoveRefusal{reason: fmt.Sprintf(format, args...)}
}

// LeaderMoveOptions tunes the forced-election loop. The zero value is the
// production configuration; tests shorten it.
type LeaderMoveOptions struct {
	// PollInterval is how often the node list is re-read while waiting.
	PollInterval time.Duration
	// SettleTimeout bounds one election: how long the new leader may take to
	// appear after the old one was demoted.
	SettleTimeout time.Duration
	// ElectionGrace is how long the demoted leader stays a worker before it is
	// promoted back. A node that is demoted cannot list nodes (it is not a
	// manager any more), so the caller cannot watch the election from there;
	// this covers the election window instead.
	ElectionGrace time.Duration
	// MaxElections caps how many forced elections one call may perform. The
	// swarm picks the winner, so a move to a specific manager may need more
	// than one; three is enough in practice and keeps a pathological cluster
	// from being churned indefinitely.
	MaxElections int
	// Budget bounds the whole call. The daemon's request timeout is 30s, so an
	// HTTP-triggered move must give up and report where leadership is instead of
	// being cut off mid-election.
	Budget time.Duration
}

func (o LeaderMoveOptions) withDefaults() LeaderMoveOptions {
	if o.PollInterval <= 0 {
		o.PollInterval = 500 * time.Millisecond
	}
	if o.SettleTimeout <= 0 {
		o.SettleTimeout = 10 * time.Second
	}
	if o.ElectionGrace <= 0 {
		o.ElectionGrace = 5 * time.Second
	}
	if o.MaxElections <= 0 {
		o.MaxElections = 3
	}
	if o.Budget <= 0 {
		o.Budget = 25 * time.Second
	}
	return o
}

// MoveLeadership forces Swarm leadership onto target.
//
// Preconditions are the ones node promote/demote already refuse on, plus the one
// this operation adds: forcing an election demotes the current leader for the
// duration, so it refuses when that would leave fewer than three managers — the
// same floor a demote uses, because the risk is identical.
func MoveLeadership(ctx context.Context, dc runtime.Client, target string, opts LeaderMoveOptions) (LeaderMove, error) {
	if dc == nil {
		return LeaderMove{}, errors.New("no docker client: leadership can only be moved from a swarm manager")
	}
	opts = opts.withDefaults()

	nodes, err := dc.NodeList(ctx)
	if err != nil {
		return LeaderMove{}, fmt.Errorf("list swarm nodes: %w", err)
	}
	from := leaderHostname(nodes)
	node, ok := nodeByHostname(nodes, target)
	if !ok {
		return LeaderMove{}, refuseMove("node %q is not in this swarm (see: pmcluster node list)", target)
	}
	if node.Role != "manager" {
		return LeaderMove{}, refuseMove("refusing to move leadership to %s: it is a %s, and only a manager can win the Raft election.\n"+
			"  promote it in the swarm first:  docker node promote %s\n"+
			"  then re-run:                    pmcluster cluster leader %s",
			target, orWorker(node.Role), target, target)
	}
	q := QuorumOf(nodes)
	if why := MembershipChangeRefusal(q, "move leadership to "+target); why != "" {
		return LeaderMove{}, refuseMove("%s", why)
	}
	if q.Managers-1 < 3 {
		return LeaderMove{}, refuseMove("refusing to move leadership to %s: forcing the election demotes %s for the duration, leaving %d manager(s). "+
			"Below three, one more loss takes the swarm's quorum with it and nothing can be committed — including the move itself.\n"+
			"  add a manager first:  docker node promote <other>",
			target, orUnknown(from), q.Managers-1)
	}
	if from == "" {
		return LeaderMove{}, refuseMove("no manager reports itself as leader (the swarm may be mid-election) — retry in a few seconds")
	}

	res := LeaderMove{From: from, Leader: from}
	if from == target {
		return res, nil
	}
	deadline := time.Now().Add(opts.Budget)

	for res.Elections < opts.MaxElections && time.Now().Before(deadline) {
		nodes, err = dc.NodeList(ctx)
		if err != nil {
			return res, fmt.Errorf("list swarm nodes: %w", err)
		}
		cur := leaderNode(nodes)
		if cur == nil || cur.Hostname == "" {
			return res, refuseMove("no manager reports itself as leader (the swarm may be mid-election) — retry in a few seconds")
		}
		if cur.Hostname == target {
			res.Leader = target
			return res, nil
		}

		// Demote the leader: Raft removes it from the configuration and the
		// survivors elect a replacement.
		if err := dc.SetNodeRole(ctx, cur.ID, "worker"); err != nil {
			return res, fmt.Errorf("demote leader %s to force an election: %w", cur.Hostname, err)
		}
		if err := sleepCtx(ctx, opts.ElectionGrace); err != nil {
			// The context died mid-move: put the manager back before leaving,
			// or the cluster is left one manager short.
			restoreErr := dc.SetNodeRole(context.WithoutCancel(ctx), cur.ID, "manager")
			if restoreErr != nil {
				return res, fmt.Errorf("%v; restoring %s to manager also failed (%v) — it is a WORKER now, promote it: docker node promote %s",
					err, cur.Hostname, restoreErr, cur.Hostname)
			}
			return res, err
		}
		// Always put the demoted manager back, whatever the election did.
		if err := dc.SetNodeRole(ctx, cur.ID, "manager"); err != nil {
			return res, fmt.Errorf("restoring %s to manager failed after forcing an election: %w\n  the node is a WORKER now; promote it: docker node promote %s",
				cur.Hostname, err, cur.Hostname)
		}
		next, err := waitForLeaderChange(ctx, dc, cur.Hostname, opts)
		if err != nil {
			return res, err
		}
		res.Elections++
		if next == target {
			res.Leader = target
			return res, nil
		}
	}

	// Report where leadership actually is. The swarm elects its own leader;
	// pretending the move succeeded would send the operator to test failover on
	// the wrong node.
	nodes, err = dc.NodeList(ctx)
	if err == nil {
		res.Leader = leaderHostname(nodes)
	}
	return res, fmt.Errorf("leadership moved %d time(s) but landed on %s, not %s — the swarm elects its own leader, so re-run the command to try again",
		res.Elections, orUnknown(res.Leader), target)
}

// waitForLeaderChange polls the node list until a leader other than previous is
// reported, or the settle timeout expires.
func waitForLeaderChange(ctx context.Context, dc runtime.Client, previous string, opts LeaderMoveOptions) (string, error) {
	deadline := time.Now().Add(opts.SettleTimeout)
	for {
		nodes, err := dc.NodeList(ctx)
		if err != nil {
			return "", fmt.Errorf("watch the election after demoting %s: %w", previous, err)
		}
		if h := leaderHostname(nodes); h != "" && h != previous {
			return h, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("demoting %s produced no new leader within %s — the remaining managers may not be able to reach each other; check `docker node list`",
				previous, opts.SettleTimeout)
		}
		if err := sleepCtx(ctx, opts.PollInterval); err != nil {
			return "", err
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func leaderNode(nodes []runtime.Node) *runtime.Node {
	for i := range nodes {
		if nodes[i].IsLeader {
			return &nodes[i]
		}
	}
	return nil
}

func leaderHostname(nodes []runtime.Node) string {
	if n := leaderNode(nodes); n != nil {
		return n.Hostname
	}
	return ""
}

func nodeByHostname(nodes []runtime.Node, hostname string) (runtime.Node, bool) {
	for _, n := range nodes {
		if n.Hostname == hostname {
			return n, true
		}
	}
	return runtime.Node{}, false
}

func orWorker(role string) string {
	if role == "" {
		return "node with no role"
	}
	return role
}

func orUnknown(host string) string {
	if host == "" {
		return "(unknown node)"
	}
	return host
}
