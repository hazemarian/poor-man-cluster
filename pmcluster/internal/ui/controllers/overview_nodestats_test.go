package controllers

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

// nodeStatsBody builds an /api/node-stats body from rows, so each test states
// the numbers it renders instead of inheriting a shared fixture.
func nodeStatsBody(rows ...string) string {
	return `{"node_stats":[` + strings.Join(rows, ",") + `]}`
}

func nodeStatJSON(host, role string, cores int, load float64, memTotal, memUsed, memAvail int64, dockerUp bool, sampleTS int64) string {
	return fmt.Sprintf(`{"hostname":%q,"role":%q,"cores":%d,"load1":%v,"load5":%v,"load15":%v,`+
		`"mem_total_mb":%d,"mem_used_mb":%d,"mem_avail_mb":%d,"docker_up":%t,`+
		`"top_rss":[{"pid":101,"name":"pmcluster","rss_mb":902,"cpu_pct":3.1}],`+
		`"top_cpu":[{"pid":101,"name":"pmcluster","rss_mb":902,"cpu_pct":37.5}],`+
		`"sample_ts":%d,"updated_at":%d}`,
		host, role, cores, load, load, load,
		memTotal, memUsed, memAvail, dockerUp, sampleTS, sampleTS)
}

// TestOverviewRendersPerNodeResources drives the fragment through the real
// renderer: the panel must show, for each node, its load, its memory, its top
// processes and the sample's age.
func TestOverviewRendersPerNodeResources(t *testing.T) {
	now := time.Now().Unix()
	nodeStatsFixture = nodeStatsBody(
		nodeStatJSON("nxt-sw-5-m", "manager", 2, 9.96, 1839, 1639, 200, true, now),
		nodeStatJSON("nxt-sw-4-m", "worker", 1, 0.42, 1839, 700, 1139, true, now),
	)
	h := newCtrlHarness(t)
	h.mountOverview()
	body := h.get(t, "/web/overview").Body.String()

	for _, want := range []string{
		"nxt-sw-5-m", "nxt-sw-4-m",
		"9.96", "0.42",
		"1,639 / 1,839 MB", "700 / 1,839 MB",
		"pmcluster", "902 MB", "37.5%",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("overview body is missing %q", want)
		}
	}
	// A live row must NOT be greyed or labelled stale.
	if strings.Contains(body, "is-stale") {
		t.Error("a fresh sample rendered as stale")
	}
}

func TestOverviewGreysAndLabelsAStaleRow(t *testing.T) {
	now := time.Now().Unix()
	nodeStatsFixture = nodeStatsBody(
		nodeStatJSON("fresh-node", "manager", 2, 0.5, 1839, 700, 1139, true, now),
		nodeStatJSON("silent-node", "worker", 1, 0.1, 1839, 700, 1139, true, now-600),
	)
	h := newCtrlHarness(t)
	h.mountOverview()
	body := h.get(t, "/web/overview").Body.String()

	// The stale row carries the tint class AND the word: the rule is "greyed
	// AND labelled", so both must be present.
	if !strings.Contains(body, `class="is-stale"`) {
		t.Error("stale row is not greyed (no is-stale class)")
	}
	if !strings.Contains(body, "stale") {
		t.Error("stale row is not labelled")
	}
	// The label belongs to the stale row's age cell, and only one row is greyed.
	if n := strings.Count(body, `class="is-stale"`); n != 1 {
		t.Errorf("is-stale appears %d times, want exactly the one stale row", n)
	}
}

func TestOverviewEmptyNodeStatsSaysSo(t *testing.T) {
	nodeStatsFixture = `{"node_stats":[]}`
	h := newCtrlHarness(t)
	h.mountOverview()
	res := h.get(t, "/web/overview")
	body := res.Body.String()

	if res.Code != 200 {
		t.Fatalf("status = %d, want 200 (an empty table is not an error)", res.Code)
	}
	if !strings.Contains(strings.ToLower(body), "no node stats") {
		t.Error("empty node-stats table must say 'no node stats'")
	}
	// And it must not dress the empty state up as a failure.
	if strings.Contains(body, "banner bad") {
		t.Error("empty node-stats rendered an error banner")
	}
}

// TestNodeStatTintBoundaries pins each threshold at the value that flips it.
// These are the four rules the operator named; an off-by-one here is a console
// that either cries wolf or stays silent on the node that is dying.
func TestNodeStatTintBoundaries(t *testing.T) {
	now := time.Now()
	base := pmapi.NodeStat{
		Hostname: "n1", Role: "manager", Cores: 2,
		MemTotalMB: 1000, MemUsedMB: 850, MemAvailMB: 150,
		DockerUp: true, SampleTS: now.Unix(),
	}

	cases := []struct {
		name  string
		mut   func(*pmapi.NodeStat)
		check func(nodeStatModel) bool
		want  bool
	}{
		{"load exactly at 1.5x cores is not overloaded", func(s *pmapi.NodeStat) { s.Load1 = 3.0 }, func(m nodeStatModel) bool { return m.Overloaded }, false},
		{"load just over 1.5x cores is overloaded", func(s *pmapi.NodeStat) { s.Load1 = 3.01 }, func(m nodeStatModel) bool { return m.Overloaded }, true},
		{"available exactly 15% is not low", func(s *pmapi.NodeStat) { s.MemAvailMB = 150 }, func(m nodeStatModel) bool { return m.MemLow }, false},
		{"available under 15% is low", func(s *pmapi.NodeStat) { s.MemAvailMB = 149 }, func(m nodeStatModel) bool { return m.MemLow }, true},
		{"sample exactly 3m old is not stale", func(s *pmapi.NodeStat) { s.SampleTS = now.Unix() - 180 }, func(m nodeStatModel) bool { return m.Stale }, false},
		{"sample 3m1s old is stale", func(s *pmapi.NodeStat) { s.SampleTS = now.Unix() - 181 }, func(m nodeStatModel) bool { return m.Stale }, true},
		{"docker down tints", func(s *pmapi.NodeStat) { s.DockerUp = false }, func(m nodeStatModel) bool { return m.DockerDown }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.mut(&s)
			got := tc.check(newNodeStatModel(s, now))
			if got != tc.want {
				t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestNodeStatRowClassPrefersStale stops an aged reading from rendering as a
// current alarm: staleness wins over the live tints.
func TestNodeStatRowClassPrefersStale(t *testing.T) {
	now := time.Now()
	s := pmapi.NodeStat{
		Hostname: "n1", Cores: 1, Load1: 99, MemTotalMB: 100, MemAvailMB: 1,
		DockerUp: false, SampleTS: now.Unix() - 3600,
	}
	if got := newNodeStatModel(s, now).RowClass(); got != "is-stale" {
		t.Fatalf("RowClass = %q, want is-stale", got)
	}
	s.SampleTS = now.Unix()
	if got := newNodeStatModel(s, now).RowClass(); got != "is-hot" {
		t.Fatalf("RowClass = %q, want is-hot for a live, tinted row", got)
	}
}

// TestNodeStatAgeTextKeepsTheAgeReadable covers the compact ages the panel
// prints, including the "no timestamp" case which must not read as 0s.
func TestNodeStatAgeTextKeepsTheAgeReadable(t *testing.T) {
	now := time.Now()
	cases := []struct {
		ts   int64
		want string
	}{
		{now.Unix() - 8, "8s"},
		{now.Unix() - 240, "4m"},
		{now.Unix() - 7200, "2h"},
		{0, "—"},
	}
	for _, c := range cases {
		m := newNodeStatModel(pmapi.NodeStat{Hostname: "n1", SampleTS: c.ts}, now)
		if m.AgeText != c.want {
			t.Errorf("age for ts=%d = %q, want %q", c.ts, m.AgeText, c.want)
		}
	}
}
