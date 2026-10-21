package cluster

import (
	"context"
	"hash/fnv"
	"sort"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// Placement for the platform stacks has one rule per stack, because the stacks
// differ in whether they own data:
//
//	observability  storage round-robin — Parseable keeps a Parquet store on a
//	               host path, so it must never be rescheduled onto a node
//	               holding none of it.
//	backup         round-robin over WORKER nodes — SeaweedFS holds the durable
//	               copy, and placing it outside the storage pool keeps a backup
//	               out of the failure domain it protects.
//	sso            no data at all, so no hostname: role-based (node.role ==
//	               manager). Pinning it to a storage node would be a lie about
//	               what it needs.
//	edge           follows leadership via node.labels.pmcluster.leader.
//	infra          role-based (node.role == manager), unchanged.
//
// The two hostname pins are only as stable as the node list they hash, which is
// why the choice is derived from storage_nodes (a settled, operator-owned list)
// and the worker set — not from whatever the scheduler felt like.

// roundRobinHost mirrors stacks.roundRobinNode: the same stack name always maps
// to the same host, and adding a stack never reshuffles the existing ones. It is
// duplicated here rather than imported because the stacks package's own tests
// import this one, and an import in this direction would close the cycle.
func roundRobinHost(name string, nodes []string) string {
	if len(nodes) == 0 {
		return ""
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return nodes[int(h.Sum32())%len(nodes)]
}

// StorageNodesFor returns the storage_nodes setting as a stable, sorted slice.
func StorageNodesFor(ctx context.Context, st *store.Store) []string {
	nodes := splitStorageNodes(st.SettingDefault(ctx, SettingStorageNodes(), ""))
	sort.Strings(nodes)
	return nodes
}

// LeaderNode is the hostname of the current Swarm leader — the node the console
// follows. Resolved fresh at every render, so an election relocates the console
// on the next reconcile pass instead of leaving it on a node that no longer
// holds the API.
func LeaderNode(ctx context.Context, dc runtime.Client) string {
	if dc == nil {
		return ""
	}
	nodes, err := dc.NodeList(ctx)
	if err != nil {
		return ""
	}
	for _, n := range nodes {
		if n.IsLeader && n.Hostname != "" {
			return n.Hostname
		}
	}
	return ""
}

// ObservabilityNode is the cluster standard for the observability stack: the
// storage round-robin, recorded on first resolution so it never moves again.
// Parseable's Parquet store is a host path — re-resolving it after a
// storage_nodes change would bring it up beside an empty directory.
func ObservabilityNode(ctx context.Context, st *store.Store) string {
	return pinnedOrResolved(ctx, st, "stack_pin_"+string(StackObservability), func() string {
		return roundRobinHost(string(StackObservability), StorageNodesFor(ctx, st))
	})
}

// BackupPlacement round-robins the backup stack across WORKER nodes, recorded on
// first resolution for the same reason: the durable copy lives on a host path.
// Workers are sorted so the answer cannot move because the API returned nodes in
// a different order; a cluster with no workers falls back to "" (any node),
// which is what a single-manager install has.
func BackupPlacement(ctx context.Context, st *store.Store, dc runtime.Client) string {
	return pinnedOrResolved(ctx, st, "stack_pin_"+string(StackBackup), func() string {
		if dc == nil {
			return ""
		}
		nodes, err := dc.NodeList(ctx)
		if err != nil {
			return ""
		}
		var workers []string
		for _, n := range nodes {
			if n.Role == "worker" && n.Hostname != "" {
				workers = append(workers, n.Hostname)
			}
		}
		if len(workers) == 0 {
			return ""
		}
		sort.Strings(workers)
		return roundRobinHost(string(StackBackup), workers)
	})
}

// pinnedOrResolved returns the recorded placement for a platform stack, and
// records the resolved one the first time. The pin is what makes the choice
// durable: without it every render re-runs the round-robin, so adding or
// removing a storage node relocates a stack onto a node holding none of its
// data. Deliberately NOT in the settings allowlist — it is written here, not by
// an operator.
func pinnedOrResolved(ctx context.Context, st *store.Store, key string, resolve func() string) string {
	if st == nil {
		return resolve()
	}
	if pin := st.SettingDefault(ctx, key, ""); pin != "" {
		return pin
	}
	node := resolve()
	if node != "" {
		_ = st.SetSetting(ctx, key, node)
	}
	return node
}
