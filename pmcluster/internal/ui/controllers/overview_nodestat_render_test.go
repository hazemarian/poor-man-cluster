package controllers

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

// procJSON builds one process object for the /api/node-stats fixture.
func procJSON(pid int, name string, rssMB, cpuPct float64) string {
	return fmt.Sprintf(`{"pid":%d,"name":%q,"rss_mb":%v,"cpu_pct":%v}`, pid, name, rssMB, cpuPct)
}

// nodeStatWithProcs builds one node row with explicit top lists, so a test
// states the processes it renders instead of inheriting the shared fixture.
func nodeStatWithProcs(host, role string, rss, cpu []string) string {
	now := time.Now().Unix()
	return fmt.Sprintf(`{"hostname":%q,"role":%q,"cores":2,"load1":1.1,"load5":1.0,"load15":0.9,`+
		`"mem_total_mb":3828,"mem_used_mb":1820,"mem_avail_mb":2008,"docker_up":true,`+
		`"top_rss":[%s],"top_cpu":[%s],"sample_ts":%d,"updated_at":%d}`,
		host, role, strings.Join(rss, ","), strings.Join(cpu, ","), now, now)
}

// nodestatTable cuts the desktop table out of the rendered fragment. The panel
// also renders a mobile key/value view of the same data; a structural
// assertion must not accidentally count both.
func nodestatTable(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `class="table nodestat-table"`)
	if start < 0 {
		t.Fatal("overview body has no nodestat table")
	}
	end := strings.Index(body[start:], "</table>")
	if end < 0 {
		t.Fatal("nodestat table is not closed")
	}
	return body[start : start+end]
}

// nodestatRow returns the table row that starts with host, so height and badge
// assertions compare rows rather than the whole table.
func nodestatRow(t *testing.T, table, host string) string {
	t.Helper()
	idx := strings.Index(table, ">"+host+"<")
	if idx < 0 {
		t.Fatalf("nodestat table has no row for %s", host)
	}
	rowStart := strings.LastIndex(table[:idx], "<tr ")
	rowEnd := strings.Index(table[idx:], "</tr>")
	if rowStart < 0 || rowEnd < 0 {
		t.Fatalf("cannot delimit the row for %s", host)
	}
	return table[rowStart : idx+rowEnd]
}

var procRowRe = regexp.MustCompile(`<li class="proc-row">.*?</li>`)

// TestNodestatProcRowsSeparateNameAndValue: the panel rendered `parseable439
// MB` — a name span and a value span with nothing between them. Every rendered
// line must carry a separator element and the value must sit in the shared
// numeric class, so numbers line up on one axis instead of starting wherever
// the name ends.
func TestNodestatProcRowsSeparateNameAndValue(t *testing.T) {
	nodeStatsFixture = nodeStatsBody(nodeStatWithProcs("nxt-sw-5-m", "manager",
		[]string{procJSON(101, "parseable", 439, 12.5), procJSON(102, "docker", 252, 3.2)},
		[]string{procJSON(101, "parseable", 439, 12.5), procJSON(102, "docker", 252, 3.2)}))
	h := newCtrlHarness(t)
	h.mountOverview()
	table := nodestatTable(t, h.get(t, "/web/overview").Body.String())

	rows := procRowRe.FindAllString(table, -1)
	if len(rows) == 0 {
		t.Fatal("no process rows rendered")
	}
	for _, row := range rows {
		for _, want := range []string{`class="p-name"`, `class="p-sep"`, `class="p-val num`} {
			if !strings.Contains(row, want) {
				t.Errorf("process row %q is missing %s", row, want)
			}
		}
		// The separator must be a real element with content, not an empty span.
		if !strings.Contains(row, `<span class="p-sep" aria-hidden="true">·</span>`) {
			t.Errorf("process row has no visible separator: %q", row)
		}
		if strings.Index(row, `class="p-sep"`) > strings.Index(row, `class="p-val`) {
			t.Errorf("value precedes the separator: %q", row)
		}
	}
	// The memory cell separates the used/total pair from the free figure.
	if !strings.Contains(table, `class="mem-sep"`) {
		t.Error("memory cell has no separator before the free figure")
	}
	if !strings.Contains(table, `1,820 / 3,828 MB`) {
		t.Error("memory pair is not rendered as used / total")
	}
}

// TestNodestatAggregatesSameNameProcesses: two PIDs called `parseable` are one
// program to the operator. They must render as one line whose value is the sum
// (439 + 231 = 670 MB) — not two unexplained duplicates.
func TestNodestatAggregatesSameNameProcesses(t *testing.T) {
	nodeStatsFixture = nodeStatsBody(nodeStatWithProcs("nxt-sw-5-m", "manager",
		[]string{
			procJSON(101, "parseable", 439, 10),
			procJSON(102, "parseable", 231, 2),
			procJSON(103, "docker", 252, 3),
		},
		[]string{
			procJSON(104, "docker", 252, 3),
		}))
	h := newCtrlHarness(t)
	h.mountOverview()
	table := nodestatTable(t, h.get(t, "/web/overview").Body.String())

	if n := strings.Count(table, `class="p-name">parseable<`); n != 1 {
		t.Fatalf("`parseable` rendered %d times in the table, want one aggregated row", n)
	}
	if !strings.Contains(table, "670 MB") {
		t.Error("aggregated row does not show the summed RSS (439 + 231 = 670 MB)")
	}
	if strings.Contains(table, "231 MB") {
		t.Error("a folded PID still renders its own row (231 MB)")
	}

	// The model is where the decision lives: one entry, sum, count of 2.
	m := newNodeStatModel(pmapi.NodeStat{
		Hostname: "n1", Role: "manager", Cores: 2, MemTotalMB: 100, MemAvailMB: 20, DockerUp: true,
		SampleTS: time.Now().Unix(),
		TopRSS: []pmapi.NodeStatProc{
			{PID: 101, Name: "parseable", RSSMB: 439},
			{PID: 102, Name: "parseable", RSSMB: 231},
		},
	}, time.Now())
	if len(m.TopRSS) != 1 {
		t.Fatalf("aggregated rows = %d, want 1", len(m.TopRSS))
	}
	if m.TopRSS[0].Value != "670 MB" || m.TopRSS[0].Count != 2 {
		t.Fatalf("aggregated row = %+v, want 670 MB over 2 processes", m.TopRSS[0])
	}
}

// TestNodestatRendersThreeOfFive: the data keeps five processes and the panel
// renders three. Truncation is a render decision, so the model must still hold
// the full list.
func TestNodestatRendersThreeOfFive(t *testing.T) {
	var rss, cpu []string
	for i := 1; i <= 5; i++ {
		rss = append(rss, procJSON(100+i, fmt.Sprintf("proc%d", i), float64(500-10*i), float64(10-i)))
		cpu = append(cpu, procJSON(100+i, fmt.Sprintf("proc%d", i), float64(500-10*i), float64(10-i)))
	}
	nodeStatsFixture = nodeStatsBody(nodeStatWithProcs("nxt-sw-5-m", "manager", rss, cpu))
	h := newCtrlHarness(t)
	h.mountOverview()
	table := nodestatTable(t, h.get(t, "/web/overview").Body.String())

	if n := strings.Count(table, `class="proc-row">`); n != 6 {
		t.Errorf("rendered process rows = %d, want 3 per column (6 for two columns)", n)
	}
	if strings.Contains(table, `class="p-name">proc4<`) || strings.Contains(table, `class="p-name">proc5<`) {
		t.Error("a fourth/fifth process rendered; the preview must stop at three")
	}

	m := newNodeStatModel(pmapi.NodeStat{
		Hostname: "n1", Cores: 2, MemTotalMB: 100, MemAvailMB: 20, DockerUp: true,
		SampleTS: time.Now().Unix(),
		TopRSS: []pmapi.NodeStatProc{
			{Name: "a", RSSMB: 50}, {Name: "b", RSSMB: 40}, {Name: "c", RSSMB: 30},
			{Name: "d", RSSMB: 20}, {Name: "e", RSSMB: 10},
		},
	}, time.Now())
	if len(m.TopRSS) != 5 {
		t.Fatalf("model kept %d processes, want 5 (data must keep what the panel truncates)", len(m.TopRSS))
	}
	if len(m.TopRSSSlots) != procPreviewLimit {
		t.Fatalf("render slots = %d, want %d", len(m.TopRSSSlots), procPreviewLimit)
	}
}

// TestNodestatRoleBadgeOnEveryRow: the role badge used to appear on managers
// and vanish on workers, leaving a ragged leading column. Every row carries the
// same pill treatment.
func TestNodestatRoleBadgeOnEveryRow(t *testing.T) {
	one := []string{procJSON(101, "pmcluster", 120, 4)}
	nodeStatsFixture = nodeStatsBody(
		nodeStatWithProcs("nxt-sw-1-m", "manager", one, one),
		nodeStatWithProcs("nxt-sw-3-w", "worker", one, one),
		// An empty role used to render nothing at all: the badge must still
		// occupy the slot, or the column reads as inconsistent.
		nodeStatWithProcs("nxt-sw-9-x", "", one, one),
	)
	h := newCtrlHarness(t)
	h.mountOverview()
	table := nodestatTable(t, h.get(t, "/web/overview").Body.String())

	if n := strings.Count(table, `class="pill plain role-pill"`); n != 3 {
		t.Errorf("role pills = %d, want one on each of the three rows", n)
	}
	worker := strings.ToLower(nodestatRow(t, table, "nxt-sw-3-w"))
	if !strings.Contains(worker, `class="pill plain role-pill">worker<`) {
		t.Errorf("worker row has no role badge: %s", worker)
	}
	manager := strings.ToLower(nodestatRow(t, table, "nxt-sw-1-m"))
	if !strings.Contains(manager, `class="pill plain role-pill">manager<`) {
		t.Errorf("manager row lost its role badge: %s", manager)
	}
	if !strings.Contains(strings.ToLower(nodestatRow(t, table, "nxt-sw-9-x")), `class="pill plain role-pill">unknown<`) {
		t.Error("a row with no reported role renders no badge")
	}
}

// TestNodestatRowHeightIsIndependentOfProcessCount: a one-process node and a
// five-process node must occupy the same three lines, or every sample arrival
// resizes the table. The markup guarantees it with fixed three-slot lists; the
// CSS pins the same row count.
func TestNodestatRowHeightIsIndependentOfProcessCount(t *testing.T) {
	five := []string{
		procJSON(101, "a", 50, 5), procJSON(102, "b", 40, 4), procJSON(103, "c", 30, 3),
		procJSON(104, "d", 20, 2), procJSON(105, "e", 10, 1),
	}
	one := []string{procJSON(201, "solo", 9, 1)}
	nodeStatsFixture = nodeStatsBody(
		nodeStatWithProcs("five-node", "manager", five, five),
		nodeStatWithProcs("one-node", "worker", one, one),
	)
	h := newCtrlHarness(t)
	h.mountOverview()
	table := nodestatTable(t, h.get(t, "/web/overview").Body.String())

	for _, host := range []string{"five-node", "one-node"} {
		row := nodestatRow(t, table, host)
		if n := strings.Count(row, `class="proc-list is-fixed"`); n != 2 {
			t.Errorf("%s: fixed-height lists = %d, want 2 (RSS and CPU)", host, n)
		}
		// Two columns, three slots each, padding included.
		if n := strings.Count(row, "class=\"proc-row"); n != 6 {
			t.Errorf("%s: process slots = %d, want 6 (2 columns x 3 fixed slots)", host, n)
		}
	}
	if !strings.Contains(nodestatRow(t, table, "one-node"), `class="proc-row is-empty"`) {
		t.Error("a one-process row does not pad its empty slots, so its height would collapse")
	}
}
