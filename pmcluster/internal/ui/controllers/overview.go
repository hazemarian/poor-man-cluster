package controllers

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

// Overview renders the app shell and the cluster overview fragment.
type Overview struct{ *Controller }

// App renders the HTMX application shell (sidebar + empty #view). All data
// loads via fragment requests from the shell itself.
//
// The shell's data comes from the renderer's ShellData builder rather than a
// hand-rolled map here: shell chrome (breadcrumb, title, language links, the
// per-request refresh URL) is exactly what that builder exists for, and a
// second copy of it drifts. Passing a map without "Crumb" is how this route
// came to stream "invalid value; expected string" into the document title.
func (c Overview) App(g *gin.Context) {
	if c.Views.ShellData == nil {
		g.Status(http.StatusInternalServerError)
		return
	}
	c.Views.Page(g, "app", c.Views.ShellData(g))
}

type overviewData struct {
	Me    *meModel
	Info  *infoModel
	Nodes []nodeModel

	// NodeStats is the per-node resource table: what is eating each machine,
	// read from the control-plane DB (never from the telemetry backend). It is
	// a separate panel from Nodes on purpose — the node list comes from the
	// swarm API and stays correct when telemetry is broken, and this one stays
	// correct when the swarm API is the thing timing out.
	NodeStats      []nodeStatModel
	NodeStatsKnown bool

	// The v1 console rendered every failed call as a zero: a broken swarm query
	// looked exactly like an empty cluster. These flags keep "we don't know"
	// separate from "the answer is none", and ErrRaw keeps the payload for the
	// operator who wants it without putting it in the reading path.
	// Apply is the last platform apply: what it changed and, more usefully, what
	// it could not do. Reconcile runs unattended, so its warnings used to live
	// in journald and nowhere else.
	Apply      *applyModel
	ApplyKnown bool

	// Skew is the manager fleet's binary versions when any of them is not the
	// version this console's daemon runs.
	Skew      []versionModel
	SkewKnown bool

	InfoKnown  bool
	NodesKnown bool
	ErrKey     string
	ErrRaw     string
}

// meModel/infoModel/nodeModel are thin view aliases so templates don't import
// the pmapi package.
type meModel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Fragment renders cluster info + nodes, or a humanised failure with the raw
// upstream payload folded away behind a disclosure.
func (c Overview) Fragment(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)

	d := overviewData{}
	if !configured {
		d.ErrKey = "err.api_not_configured"
		c.Views.Fragment(g, "overview", d)
		return
	}

	// Five independent daemon reads. They used to run one after another, so the
	// landing fragment cost five round trips (measured 1.1-1.4s against a live
	// cluster from a ~200ms-away host) for one screen of data. They are issued
	// together now and the fragment costs its slowest read.
	var (
		me       *pmapi.Me
		err      error
		info     *pmapi.ClusterInfo
		errI     error
		versions *pmapi.FleetVersions
		errV     error
		rec      *pmapi.ApplyRecord
		errA     error
		nodes    []pmapi.Node
		errN     error
		stat     []pmapi.NodeStat
		errS     error
	)
	var wg sync.WaitGroup
	wg.Add(6)
	go func() { defer wg.Done(); me, err = c.API.Me(ctx) }()
	go func() { defer wg.Done(); info, errI = c.API.ClusterInfo(ctx) }()
	go func() { defer wg.Done(); versions, errV = c.API.Versions(ctx) }()
	go func() { defer wg.Done(); rec, errA = c.API.ApplyRecord(ctx) }()
	go func() { defer wg.Done(); nodes, errN = c.API.Nodes(ctx) }()
	go func() { defer wg.Done(); stat, errS = c.API.NodeStats(ctx) }()
	wg.Wait()

	if err == nil && me != nil {
		d.Me = &meModel{ID: me.ID, Name: me.Name}
	}
	if errI == nil && info != nil {
		d.InfoKnown = true
		d.Info = &infoModel{
			NodeName: info.NodeName, ServerVersion: info.ServerVersion,
			OS: info.OS, Arch: info.Arch, CPUs: info.CPUs, MemoryBytes: info.MemoryBytes,
			SwarmState: info.Swarm.State, SwarmManagers: info.Swarm.Managers,
			SwarmNodes: info.Swarm.Nodes, ControlAvailable: info.Swarm.ControlAvailable,
		}
	}
	if errV == nil && versions != nil {
		d.SkewKnown = true
		for _, e := range versions.Stale {
			d.Skew = append(d.Skew, versionModel{Hostname: e.Hostname, Version: e.Version})
		}
	}
	if errA == nil && rec != nil && rec.Present {
		d.ApplyKnown = true
		d.Apply = &applyModel{
			At: rec.At, Source: rec.Source, Version: rec.Version,
			Line: rec.Line, Warnings: rec.Warnings,
		}
	}
	if errN == nil {
		d.NodesKnown = true
		for _, n := range nodes {
			d.Nodes = append(d.Nodes, nodeModel{
				Hostname: n.Hostname, Role: n.Role, Status: n.Status,
				IsLeader: n.IsLeader, Availability: n.Availability, EngineVersion: n.EngineVersion,
				Storage: n.Storage,
			})
		}
	}

	if errS == nil {
		d.NodeStatsKnown = true
		now := time.Now()
		for _, s := range stat {
			d.NodeStats = append(d.NodeStats, newNodeStatModel(s, now))
		}
	}

	switch {
	case err != nil:
		d.ErrKey, d.ErrRaw = "err.api_unreachable", err.Error()
	case errI != nil:
		d.ErrKey, d.ErrRaw = "err.cluster_info", errI.Error()
	case errN != nil:
		d.ErrKey, d.ErrRaw = "err.nodes", errN.Error()
	}
	c.Views.Fragment(g, "overview", d)
}

// versionModel is one manager running a different binary version.
type versionModel struct {
	Hostname string
	Version  string
}

// applyModel is the last platform apply as the overview renders it.
type applyModel struct {
	At       string
	Source   string
	Version  string
	Line     string
	Warnings []string
}

// HasWarnings reports whether the last apply degraded, which the template uses
// to choose between a quiet line and a banner.
func (a *applyModel) HasWarnings() bool { return a != nil && len(a.Warnings) > 0 }

// AtText renders the apply's timestamp for the banner. The API hands back the
// stored time as RFC3339; a console line saying `2026-11-08T22:41:07.123456Z`
// is a payload, not a reading, so it is parsed and formatted here. An
// unparseable value falls back to the raw string rather than hiding when the
// apply happened.
func (a *applyModel) AtText() string {
	if a == nil || strings.TrimSpace(a.At) == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339Nano, a.At)
	if err != nil {
		return a.At
	}
	return t.UTC().Format("2006-01-02 15:04 MST")
}

type infoModel struct {
	NodeName         string
	ServerVersion    string
	OS               string
	Arch             string
	CPUs             int
	MemoryBytes      int64
	SwarmState       string
	SwarmManagers    int
	SwarmNodes       int
	ControlAvailable bool
}

// staleAfter is how old a sample may be before the panel greys it. The samplers
// run every 15s on every node and the leader reads the file every 15s, so three
// minutes is twelve missed ticks — an outage, not a slow pass.
const staleAfter = 3 * time.Minute

// lowMemoryPercent is the tint boundary for "this node is about to swap": less
// than 15% of RAM still available.
const lowMemoryPercent = 15

// loadPerCore is the load1/cores ratio above which a node is tinted. 1.5 is one
// and a half runnable processes per core: work is already queueing.
const loadPerCore = 1.5

// nodeStatModel is one rendered row: the numbers plus the four tint decisions,
// computed here so the template holds no thresholds (and so each one is tested
// at its boundary).
type nodeStatModel struct {
	Hostname string
	Role     string
	Cores    int

	Load1  float64
	Load5  float64
	Load15 float64

	MemTotalMB int64
	MemUsedMB  int64
	MemAvailMB int64

	TopRSS []procModel
	TopCPU []procModel

	// The rendered window. The full lists stay in TopRSS/TopCPU (up to
	// procDataLimit) so truncation is a render decision, never data loss.
	TopRSSSlots []procModel
	TopCPUSlots []procModel

	// AgeText is a compact "how old is this sample" string ("8s", "4m",
	// "2h"). Stale is true past staleAfter and both greys the row and labels
	// it — never either alone: a greyed row without a word is a broken console,
	// and a label on a live row is a lie.
	AgeText string
	Stale   bool

	// The three derived tint rules.
	Overloaded bool // load1 > 1.5 x cores
	MemLow     bool // available < 15% of total
	DockerDown bool // the node's own engine socket did not answer
}

// procModel is one process line under the two top lists.
//
// Processes that share a name are ONE row: RSS and CPU are summed, because the
// operator's question is "what is eating this node", and two `parseable` rows
// (439 MB + 231 MB) read as a rendering bug until you find the PIDs. Count
// says how many processes the row stands for, so the sum is never a mystery.
type procModel struct {
	Name  string
	Value string // pre-formatted: "902 MB" or "37%"
	Count int    // processes folded into this row; 0 on a padding slot
}

// procDataLimit is how many aggregated rows the model keeps (the sampler's own
// top-5 cap) and procPreviewLimit is how many the panel renders. Three lines
// per column keep a row short enough to scan; the data still holds five so the
// "2 more" note is truthful.
const (
	procDataLimit    = 5
	procPreviewLimit = 3
)

// unnamedProc stands in for a process whose comm is empty. It is a glyph, not
// prose: a blank name must not merge unrelated processes into one row.
const unnamedProc = "?"

// aggregateProcs folds processes with the same name into one row, sums the
// axis value and re-ranks by that sum. Order is stable on ties so the list does
// not flicker between samples.
func aggregateProcs(procs []pmapi.NodeStatProc, value func(pmapi.NodeStatProc) float64, format func(float64) string) []procModel {
	type bucket struct {
		sum   float64
		count int
	}
	byName := make(map[string]*bucket, len(procs))
	order := make([]string, 0, len(procs))
	for _, p := range procs {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = unnamedProc
		}
		b, seen := byName[name]
		if !seen {
			b = &bucket{}
			byName[name] = b
			order = append(order, name)
		}
		b.sum += value(p)
		b.count++
	}
	sort.SliceStable(order, func(i, j int) bool {
		return byName[order[i]].sum > byName[order[j]].sum
	})
	if len(order) > procDataLimit {
		order = order[:procDataLimit]
	}
	out := make([]procModel, 0, len(order))
	for _, name := range order {
		b := byName[name]
		out = append(out, procModel{Name: name, Value: format(b.sum), Count: b.count})
	}
	return out
}

// procSlots is the fixed-height window the template renders: always
// procPreviewLimit entries, padded with empty rows (Name == ""). A node with
// one process and a node with five therefore lay out the same number of lines,
// so the table does not judder as samples arrive.
func procSlots(rows []procModel) []procModel {
	slots := make([]procModel, procPreviewLimit)
	copy(slots, rows)
	return slots
}

// RowClass is the row's tint class. Staleness wins over the live tints: an old
// reading must not look like a current alarm.
func (n nodeStatModel) RowClass() string {
	switch {
	case n.Stale:
		return "is-stale"
	case n.Overloaded || n.MemLow || n.DockerDown:
		return "is-hot"
	default:
		return ""
	}
}

// newNodeStatModel turns one API row into the view model. now is passed in so
// the boundary tests do not race the clock.
func newNodeStatModel(s pmapi.NodeStat, now time.Time) nodeStatModel {
	m := nodeStatModel{
		Hostname: s.Hostname, Role: s.Role, Cores: s.Cores,
		Load1: s.Load1, Load5: s.Load5, Load15: s.Load15,
		MemTotalMB: s.MemTotalMB, MemUsedMB: s.MemUsedMB, MemAvailMB: s.MemAvailMB,
	}
	age := now.Unix() - s.SampleTS
	if s.SampleTS == 0 || age < 0 {
		// No timestamp at all is not "fresh": it is unknowable, and the panel
		// says so rather than printing 0s.
		m.Stale = true
		m.AgeText = "—"
	} else {
		m.AgeText = ageText(time.Duration(age) * time.Second)
		m.Stale = age > int64(staleAfter/time.Second)
	}
	if s.Cores > 0 {
		m.Overloaded = s.Load1 > loadPerCore*float64(s.Cores)
	}
	if s.MemTotalMB > 0 {
		m.MemLow = s.MemAvailMB*100/s.MemTotalMB < lowMemoryPercent
	}
	m.DockerDown = !s.DockerUp
	m.TopRSS = aggregateProcs(s.TopRSS,
		func(p pmapi.NodeStatProc) float64 { return p.RSSMB },
		func(v float64) string { return fmt.Sprintf("%.0f MB", v) })
	m.TopCPU = aggregateProcs(s.TopCPU,
		func(p pmapi.NodeStatProc) float64 { return p.CPUPct },
		func(v float64) string { return fmt.Sprintf("%.1f%%", v) })
	m.TopRSSSlots = procSlots(m.TopRSS)
	m.TopCPUSlots = procSlots(m.TopCPU)
	return m
}

// ageText renders a sample age compactly: 8s, 4m, 2h, 3d.
func ageText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

type nodeModel struct {
	Hostname      string
	Role          string
	Status        string
	IsLeader      bool
	Availability  string
	EngineVersion string
	Storage       bool
}
