package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodestat"
)

func nodeStat(host, role string, ts int64, load float64) nodestat.Stat {
	return nodestat.Stat{
		Hostname: host, Role: role, Cores: 2,
		Load1: load, Load5: load, Load15: load,
		MemTotalMB: 1839, MemUsedMB: 1600, MemAvailMB: 239,
		DockerUp: 1,
		TopRSS:   []nodestat.Proc{{PID: 101, Name: "pmcluster", RSSMB: 902}},
		TopCPU:   []nodestat.Proc{{PID: 202, Name: "sshd", CPUPct: 12.5}},
		SampleTS: ts,
	}
}

// TestNodeStats_OneRowPerNodeAcrossManyTicks is the size guarantee the operator
// asked for: the table is O(nodes) forever, no matter how often the leader
// writes. If this ever fails, a 2GB box grows a metrics history in the store
// every request reads from.
func TestNodeStats_OneRowPerNodeAcrossManyTicks(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	for tick := int64(0); tick < 50; tick++ {
		if err := st.UpsertNodeStats(ctx, []nodestat.Stat{
			nodeStat("n1", "manager", 1000+tick, float64(tick)),
			nodeStat("n2", "worker", 1000+tick, float64(tick)),
		}, 5000+tick); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
	}

	rows, err := st.ListNodeStats(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d after 50 ticks, want 2 (one per node)", len(rows))
	}
	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM node_stats`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("node_stats holds %d rows, want 2", count)
	}
	// The row carries the NEWEST sample, not the first.
	if rows[0].Load1 != 49 || rows[0].SampleTS != 1049 {
		t.Fatalf("n1 row = load %v ts %d, want the last tick (49 / 1049)", rows[0].Load1, rows[0].SampleTS)
	}
}

func TestNodeStats_RoundTripPreservesTopLists(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	in := nodeStat("n1", "manager", 1700, 1.5)
	in.TopRSS = []nodestat.Proc{
		{PID: 1, Name: "a", RSSMB: 10},
		{PID: 2, Name: "b", RSSMB: 20},
	}
	in.TopCPU = []nodestat.Proc{{PID: 3, Name: "c", CPUPct: 99.5, RSSMB: 1}}
	in.DockerUp = 0
	if err := st.UpsertNodeStats(ctx, []nodestat.Stat{in}, 1800); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rows, err := st.ListNodeStats(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	got := rows[0]
	if len(got.TopRSS) != 2 || got.TopRSS[1].Name != "b" || got.TopRSS[1].RSSMB != 20 {
		t.Fatalf("top rss roundtrip = %+v", got.TopRSS)
	}
	if len(got.TopCPU) != 1 || got.TopCPU[0].CPUPct != 99.5 {
		t.Fatalf("top cpu roundtrip = %+v", got.TopCPU)
	}
	if got.DockerUp != 0 {
		t.Fatalf("docker_up = %d, want 0 (a dead engine must survive the round trip)", got.DockerUp)
	}
	if got.UpdatedAt != 1800 || got.SampleTS != 1700 || got.Cores != 2 {
		t.Fatalf("row = %+v", got)
	}
}

// TestNodeStats_SurviveRestart proves the rows are in the database, not in a
// process-global map: a leader that restarts (systemd upgrade, OOM) must come
// back with the table it had.
func TestNodeStats_SurviveRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.db")
	ctx := context.Background()

	first, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := first.UpsertNodeStats(ctx, []nodestat.Stat{nodeStat("n1", "manager", 100, 1)}, 200); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	rows, err := second.ListNodeStats(ctx)
	if err != nil {
		t.Fatalf("list after reopen: %v", err)
	}
	if len(rows) != 1 || rows[0].Hostname != "n1" || rows[0].MemTotalMB != 1839 {
		t.Fatalf("rows after reopen = %+v, want n1 intact", rows)
	}
}

func TestNodeStats_DepartedNodeIsDeleted(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.UpsertNodeStats(ctx, []nodestat.Stat{
		nodeStat("n1", "manager", 1, 1),
		nodeStat("n2", "worker", 1, 1),
		nodeStat("gone", "worker", 1, 1),
	}, 1); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	n, err := st.DeleteNodeStatsExcept(ctx, []string{"n1", "n2"})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want 1", n)
	}
	rows, err := st.ListNodeStats(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Hostname == "gone" {
			t.Fatal("the departed node's row survived the prune")
		}
	}

	// An empty keep list (no nodes in the swarm) clears the table rather than
	// leaving stale rows on a decommissioned cluster.
	if _, err := st.DeleteNodeStatsExcept(ctx, nil); err != nil {
		t.Fatalf("prune all: %v", err)
	}
	rows, _ = st.ListNodeStats(ctx)
	if len(rows) != 0 {
		t.Fatalf("rows = %d after full prune, want 0", len(rows))
	}
}

func TestNodeStats_EmptyUpsertIsANoop(t *testing.T) {
	st := openTestStore(t)
	if err := st.UpsertNodeStats(context.Background(), nil, 1); err != nil {
		t.Fatalf("empty upsert: %v", err)
	}
	rows, err := st.ListNodeStats(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
}
