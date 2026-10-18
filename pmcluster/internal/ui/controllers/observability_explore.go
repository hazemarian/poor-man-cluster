package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

const (
	obsTabOverview = "overview"
	obsTabLogs     = "logs"
	obsTabMetrics  = "metrics"
	obsTabTraces   = "traces"

	// obsTabInvestigate loads the legacy bounded evidence set used to build an
	// investigation prompt. It is never reachable from a request parameter.
	obsTabInvestigate = "investigate"

	obsStacksPerPage = 8
	obsLogPageSize   = 50
	obsTracePageSize = 25
	obsTopServices   = 15

	chartW = 240.0
	chartH = 56.0
)

func obsTab(raw string) string {
	switch raw {
	case obsTabLogs, obsTabMetrics, obsTabTraces:
		return raw
	}
	return obsTabOverview
}

// obsChart is a server-rendered sparkline. Known=false means the source had no
// points for this series; callers show that as "no data", never as zero.
type obsChart struct {
	Known bool
	Path  string
	Area  string
	Last  float64
	Peak  float64
	Min   float64
	// Text values are formatted server-side so a gap-only series renders no
	// number at all rather than a misleading zero.
	LastText string
	PeakText string
}

type obsNodeCard struct {
	Hostname   string
	Role       string
	Status     string
	Leader     bool
	Inventory  bool
	CPU        obsChart
	Mem        obsChart
	Load       obsChart
	HasMetrics bool
}

type obsServiceMetric struct {
	Label  string
	CPUAvg float64
	CPUMax float64
	MemAvg float64
	MemMax float64
	CPUPct int
	MemPct int
	HasCPU bool
	HasMem bool

	CPUAvgText, CPUMaxText, MemAvgText, MemMaxText string
}

type obsMetrics struct {
	Queried     bool
	Unavailable bool
	Empty       bool
	Bucket      string
	Services    []obsServiceMetric
	ServicesOK  bool
}

type obsStack struct {
	Name      string
	Total     int
	Ready     int64
	Active    int64
	Unhealthy bool
	Services  []observabilityServiceRow
}

type obsPager struct {
	Page    int
	Pages   int
	HasPrev bool
	HasNext bool
	Prev    int
	Next    int
	Total   int
}

// groupStacks filters services by a substring search, groups them by stack and
// orders unhealthy stacks first so problems are never below the fold.
func groupStacks(rows []observabilityServiceRow, q string, page int) ([]obsStack, obsPager) {
	q = strings.ToLower(strings.TrimSpace(q))
	byStack := map[string]*obsStack{}
	var order []string
	for _, row := range rows {
		if q != "" && !strings.Contains(strings.ToLower(row.Path), q) {
			continue
		}
		s := byStack[row.Stack]
		if s == nil {
			s = &obsStack{Name: row.Stack}
			byStack[row.Stack] = s
			order = append(order, row.Stack)
		}
		s.Services = append(s.Services, row)
		s.Total++
		if row.Job || !row.DesiredKnown || row.Desired == 0 {
			continue
		}
		s.Active++
		if row.Ready {
			s.Ready++
		} else {
			s.Unhealthy = true
		}
	}
	stacks := make([]obsStack, 0, len(order))
	for _, name := range order {
		s := byStack[name]
		sort.SliceStable(s.Services, func(i, j int) bool {
			a, b := s.Services[i], s.Services[j]
			if (a.Deficit > 0) != (b.Deficit > 0) {
				return a.Deficit > 0
			}
			return a.Service < b.Service
		})
		stacks = append(stacks, *s)
	}
	sort.SliceStable(stacks, func(i, j int) bool {
		if stacks[i].Unhealthy != stacks[j].Unhealthy {
			return stacks[i].Unhealthy
		}
		return stacks[i].Name < stacks[j].Name
	})
	pager := obsPager{Total: len(stacks), Pages: (len(stacks) + obsStacksPerPage - 1) / obsStacksPerPage}
	if pager.Pages == 0 {
		pager.Pages = 1
	}
	pager.Page = min(max(page, 1), pager.Pages)
	pager.HasPrev, pager.HasNext = pager.Page > 1, pager.Page < pager.Pages
	pager.Prev, pager.Next = pager.Page-1, pager.Page+1
	lo := (pager.Page - 1) * obsStacksPerPage
	hi := min(lo+obsStacksPerPage, len(stacks))
	return stacks[lo:hi], pager
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

func rowString(r observability.Row, key string) string {
	if v, ok := r[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func parseBucket(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", time.RFC3339Nano} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// buildChart turns bucketed points into SVG path data. A gap wider than one
// and a half buckets breaks the line, so missing data is drawn as missing.
// yMax fixes the vertical domain (100 for percentages).
func buildChart(points map[int64]float64, w observability.Window, bucket time.Duration, yMax float64, unit string) obsChart {
	if len(points) == 0 {
		return obsChart{}
	}
	times := make([]int64, 0, len(points))
	for t := range points {
		times = append(times, t)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	start, span := float64(w.Start.Unix()), float64(w.End.Sub(w.Start)/time.Second)
	if yMax <= 0 {
		yMax = 1
	}
	c := obsChart{Known: true, Min: points[times[0]], Peak: points[times[0]]}
	var path, area strings.Builder
	var seg [][2]float64
	flush := func() {
		if len(seg) == 0 {
			return
		}
		fmt.Fprintf(&path, "M%.1f %.1f", seg[0][0], seg[0][1])
		if len(seg) == 1 {
			path.WriteString("l.01 0")
		}
		for _, p := range seg[1:] {
			fmt.Fprintf(&path, "L%.1f %.1f", p[0], p[1])
		}
		if len(seg) > 1 {
			fmt.Fprintf(&area, "M%.1f %.1f", seg[0][0], chartH)
			for _, p := range seg {
				fmt.Fprintf(&area, "L%.1f %.1f", p[0], p[1])
			}
			fmt.Fprintf(&area, "L%.1f %.1fZ", seg[len(seg)-1][0], chartH)
		}
		seg = seg[:0]
	}
	maxGap := int64(bucket/time.Second) * 3 / 2
	var prev int64
	for i, t := range times {
		v := points[t]
		c.Last = v
		c.Peak, c.Min = fmax(c.Peak, v), fmin(c.Min, v)
		if i > 0 && t-prev > maxGap {
			flush()
		}
		prev = t
		x := (float64(t) - start) / span * chartW
		x = fmin(fmax(x, 0), chartW)
		y := chartH - 2 - fmin(fmax(v/yMax, 0), 1)*(chartH-4)
		seg = append(seg, [2]float64{x, y})
	}
	flush()
	c.Path, c.Area = path.String(), area.String()
	c.LastText, c.PeakText = fmt.Sprintf("%.1f%s", c.Last, unit), fmt.Sprintf("%.1f%s", c.Peak, unit)
	return c
}

type nodeSeries struct{ cpu, mem, load map[int64]float64 }

// loadMetrics reads the per-node (and optionally per-service) aggregates and
// merges them into the daemon's node inventory. A missing or failing source
// leaves the inventory cards intact without metrics.
func (c Observability) loadMetrics(ctx context.Context, d *observabilityData, window observability.Window, withServices bool) {
	d.Metrics.Queried = true
	source, ok := c.Telemetry.(observability.MetricSource)
	if !ok || source == nil {
		d.Metrics.Unavailable = true
		d.mergeNodeCards(nil, window, 0)
		return
	}
	bucket := observability.BucketFor(window)
	d.Metrics.Bucket = bucket.String()
	q := observability.MetricQuery{Window: window, Bucket: bucket}
	res, err := source.NodeMetrics(ctx, q)
	if err != nil || res.Status == observability.StatusUnavailable {
		d.Metrics.Unavailable = true
		d.mergeNodeCards(nil, window, bucket)
		return
	}
	series := map[string]*nodeSeries{}
	for _, row := range res.Rows {
		host, metric := rowString(row, "host"), rowString(row, "metric")
		at, okT := parseBucket(rowString(row, "bucket"))
		v, okV := toFloat(row["value"])
		if host == "" || !okT || !okV {
			continue
		}
		s := series[host]
		if s == nil {
			s = &nodeSeries{cpu: map[int64]float64{}, mem: map[int64]float64{}, load: map[int64]float64{}}
			series[host] = s
		}
		switch metric {
		case observability.MetricNodeCPUUtilization:
			s.cpu[at.Unix()] = (1 - v) * 100
		case observability.MetricNodeMemUtilization:
			s.mem[at.Unix()] = v * 100
		case observability.MetricNodeLoad1m:
			s.load[at.Unix()] = v
		}
	}
	d.mergeNodeCards(series, window, bucket)
	if !withServices {
		return
	}
	sres, err := source.ServiceMetrics(ctx, q)
	if err != nil || sres.Status == observability.StatusUnavailable {
		return
	}
	d.Metrics.ServicesOK = true
	d.Metrics.Services = projectServiceMetrics(sres.Rows)
	d.Metrics.Empty = len(d.Metrics.Services) == 0 && len(series) == 0
}

func (d *observabilityData) mergeNodeCards(series map[string]*nodeSeries, window observability.Window, bucket time.Duration) {
	used := map[string]bool{}
	build := func(card *obsNodeCard, s *nodeSeries) {
		if s == nil {
			return
		}
		card.HasMetrics = true
		card.CPU = buildChart(s.cpu, window, bucket, 100, "%")
		card.Mem = buildChart(s.mem, window, bucket, 100, "%")
		loadMax := 1.0
		for _, v := range s.load {
			loadMax = fmax(loadMax, v*1.2)
		}
		card.Load = buildChart(s.load, window, bucket, loadMax, "")
	}
	if d.Health.NodesKnown {
		for _, n := range d.Health.Nodes {
			card := obsNodeCard{Hostname: n.Hostname, Role: n.Role, Status: n.Status, Leader: n.Leader, Inventory: true}
			for host, s := range series {
				if strings.EqualFold(host, n.Hostname) {
					build(&card, s)
					used[host] = true
				}
			}
			d.NodeCards = append(d.NodeCards, card)
		}
	}
	hosts := make([]string, 0, len(series))
	for host := range series {
		if !used[host] {
			hosts = append(hosts, host)
		}
	}
	sort.Strings(hosts)
	for _, host := range hosts {
		card := obsNodeCard{Hostname: host}
		build(&card, series[host])
		d.NodeCards = append(d.NodeCards, card)
	}
}

func projectServiceMetrics(rows []observability.Row) []obsServiceMetric {
	byLabel := map[string]*obsServiceMetric{}
	var order []string
	for _, row := range rows {
		label := rowString(row, "container")
		if svc := rowString(row, "service"); svc != "" {
			label = svc
			if stack := rowString(row, "stack"); stack != "" {
				label = stack + "/" + svc
			}
		}
		if label == "" {
			continue
		}
		avg, okA := toFloat(row["avg"])
		peak, okM := toFloat(row["max"])
		if !okA || !okM {
			continue
		}
		m := byLabel[label]
		if m == nil {
			m = &obsServiceMetric{Label: label}
			byLabel[label] = m
			order = append(order, label)
		}
		switch rowString(row, "metric") {
		case observability.MetricContainerCPU:
			m.CPUAvg, m.CPUMax, m.HasCPU = avg, peak, true
		case observability.MetricContainerMemoryPercent:
			m.MemAvg, m.MemMax, m.HasMem = avg, peak, true
		}
	}
	out := make([]obsServiceMetric, 0, len(order))
	for _, label := range order {
		m := byLabel[label]
		m.CPUPct, m.MemPct = pct(m.CPUMax), pct(m.MemMax)
		m.CPUAvgText, m.CPUMaxText = fmt.Sprintf("%.1f%%", m.CPUAvg), fmt.Sprintf("%.1f%%", m.CPUMax)
		m.MemAvgText, m.MemMaxText = fmt.Sprintf("%.1f%%", m.MemAvg), fmt.Sprintf("%.1f%%", m.MemMax)
		out = append(out, *m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].MemMax > out[j].MemMax })
	if len(out) > obsTopServices {
		out = out[:obsTopServices]
	}
	return out
}

func pct(v float64) int { return int(fmin(fmax(v, 0), 100) + 0.5) }

// fmin and fmax avoid the integer-only min/max helper some test files declare
// at package scope, which shadows the builtins.
func fmin(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func fmax(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func severityClass(sev string) string {
	switch strings.ToLower(sev) {
	case "error", "err", "fatal", "critical", "crit", "panic", "emerg", "alert":
		return "bad"
	case "warn", "warning":
		return "warn"
	case "debug", "trace":
		return "plain"
	}
	return "accent"
}
