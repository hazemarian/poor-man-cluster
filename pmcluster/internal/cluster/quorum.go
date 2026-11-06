package cluster

import (
	"fmt"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// Quorum summarises the manager set as this node can see it.
//
// It is an ESTIMATE, and it says so: runtime.Node exposes a node's role and
// whether its daemon reports ready, not Raft's per-manager reachability. That is
// enough for the decisions this drives — refuse a membership change the cluster
// cannot commit, and warn before a cluster becomes unable to survive one loss —
// and it is deliberately not used for anything that needs certainty.
type Quorum struct {
	Managers int
	Ready    int
}

// QuorumOf counts the managers in a node list.
func QuorumOf(nodes []runtime.Node) Quorum {
	q := Quorum{}
	for _, n := range nodes {
		if n.Role != "manager" {
			continue
		}
		q.Managers++
		if strings.EqualFold(n.Status, "ready") {
			q.Ready++
		}
	}
	return q
}

// Required is the number of managers that must be reachable to commit anything.
func (q Quorum) Required() int { return q.Managers/2 + 1 }

// OK reports whether enough managers are up to commit a change.
func (q Quorum) OK() bool { return q.Managers > 0 && q.Ready >= q.Required() }

// Fragile reports a healthy quorum that cannot survive one more loss — the
// two-manager cluster, which looks fine until it is not.
func (q Quorum) Fragile() bool { return q.OK() && q.Managers < 3 }

// String renders the estimate for a log line or an error.
func (q Quorum) String() string {
	return fmt.Sprintf("%d/%d managers ready (need %d)", q.Ready, q.Managers, q.Required())
}

// MembershipChangeRefusal returns the reason a membership change must be refused,
// or "" when it may go ahead.
//
// Swarm would fail the change itself — a Raft configuration change needs a quorum
// — but it does so as a timeout or an opaque "rpc error", minutes later. Saying it
// up front turns "the CLI hung" into "the cluster cannot commit this right now".
func MembershipChangeRefusal(q Quorum, what string) string {
	if q.Managers == 0 {
		return "this node cannot see any managers; is the swarm healthy? (docker node ls)"
	}
	if !q.OK() {
		return fmt.Sprintf("refusing to %s: the swarm has no quorum (%s) — a membership change cannot be committed until enough managers are back. "+
			"Fix the managers first (bring the down ones back, or restore a snapshot on a surviving one)", what, q)
	}
	return ""
}

// ManagerCountWarning returns a warning worth printing about the size of the
// manager set, or "".
func ManagerCountWarning(q Quorum) string {
	switch {
	case q.Managers == 1:
		return "this cluster has ONE manager: the swarm is unavailable from its first restart until it comes back, and quorum is not a safety net here"
	case q.Managers == 2:
		return "this cluster has TWO managers: either one failing takes the quorum with it — three is the smallest safe count"
	case q.Managers%2 == 0:
		return fmt.Sprintf("this cluster has %d managers (even): the extra manager adds no fault tolerance — %d is the safe next count", q.Managers, q.Managers+1)
	}
	return ""
}
