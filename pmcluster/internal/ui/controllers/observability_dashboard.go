package controllers

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/investigation"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

const (
	volW        = 600.0
	volH        = 120.0
	obsTopErrs  = 5
	obsErrFetch = 200
)

// obsBar is one stacked column of the log-volume chart. Heights are SVG units
// measured from the baseline; Y values are the top edge of each segment.
type obsBar struct {
	X, W                    float64
	InfoY, InfoH            float64
	WarnY, WarnH            float64
	ErrY, ErrH              float64
	Title                   string
	HasInfo, HasWarn, HasEr bool
}

type obsVolume struct {
	Known      bool
	Bars       []obsBar
	Total      int64
	Errors     int64
	Warns      int64
	ErrPctText string
	MaxText    string
	StartLabel string
	EndLabel   string
	ErrPct     float64
}

type obsTraceKPI struct {
	Known     bool
	Traces    int64
	ErrTraces int64
	Spans     int64
}

type obsTopError struct {
	Message string
	Service string
	Count   int
	Last    string
	Example string
}

type obsDashboard struct {
	Volume    obsVolume
	TraceKPI  obsTraceKPI
	TopErrors []obsTopError
	TopOK     bool
}

// loadDashboard runs the overview's independent telemetry reads in parallel
// under the request context. A failed read leaves only its own panel unknown.
func (c Observability) loadDashboard(ctx context.Context, d *observabilityData, window observability.Window, redactor observability.Redactor) {
	var wg sync.WaitGroup
	if stats, ok := c.Telemetry.(observability.StatsSource); ok {
		q := observability.MetricQuery{Window: window, Bucket: observability.BucketFor(window)}
		wg.Add(2)
		go func() {
			defer wg.Done()
			if res, err := stats.LogVolume(ctx, q); err == nil {
				d.Dash.Volume = buildVolume(res.Rows, window, q.Bucket)
			}
		}()
		go func() {
			defer wg.Done()
			if res, err := stats.TraceStats(ctx, q); err == nil && len(res.Rows) == 1 {
				r := res.Rows[0]
				f := func(k string) int64 { v, _ := toFloat(r[k]); return int64(v) }
				d.Dash.TraceKPI = obsTraceKPI{Known: true, Traces: f("traces"), ErrTraces: f("error_traces"), Spans: f("spans")}
			}
		}()
	}
	if logs, ok := c.Telemetry.(observability.LogSource); ok {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := logs.Logs(ctx, observability.LogQuery{Window: window, Severity: "error", Limit: obsErrFetch})
			if err != nil || res.Status == observability.StatusUnavailable {
				return
			}
			d.Dash.TopOK = true
			d.Dash.TopErrors = topErrors(redactor.Rows(res.Rows), window)
		}()
	}
	wg.Wait()
}

// topErrors folds the newest error records into templates and keeps the most
// repeated. It uses the same compaction as the investigation input, so the
// dashboard and the model agree on what counts as one message and on which
// stderr lines are really informational.
func topErrors(rows []observability.Row, window observability.Window) []obsTopError {
	in := investigation.Compact("", "", "", rows, false)
	var out []obsTopError
	for _, e := range in.Evidence {
		if e.Severity != "ERROR" && e.Severity != "FATAL" && e.Severity != "CRITICAL" {
			continue
		}
		out = append(out, obsTopError{Message: e.Message, Service: e.Service, Count: e.Count, Last: shortTime(e.Last), Example: e.Example})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	if len(out) > obsTopErrs {
		out = out[:obsTopErrs]
	}
	return out
}

// buildVolume stacks error, warning and info counts per bucket. Empty buckets
// draw nothing, so a quiet interval is visible as a gap rather than a zero bar.
func buildVolume(rows []observability.Row, w observability.Window, bucket time.Duration) obsVolume {
	type counts struct{ info, warn, err int64 }
	by := map[int64]*counts{}
	var v obsVolume
	for _, r := range rows {
		t, ok := parseBucket(rowString(r, "bucket"))
		n, ok2 := toFloat(r["n"])
		if !ok || !ok2 {
			continue
		}
		cn := by[t.Unix()]
		if cn == nil {
			cn = &counts{}
			by[t.Unix()] = cn
		}
		switch rowString(r, "level") {
		case "error":
			cn.err += int64(n)
			v.Errors += int64(n)
		case "warn":
			cn.warn += int64(n)
			v.Warns += int64(n)
		default:
			cn.info += int64(n)
		}
		v.Total += int64(n)
	}
	v.Known = true
	// Over a day or more, a bare clock time on both ends is ambiguous.
	layout := "15:04"
	if w.End.Sub(w.Start) >= 20*time.Hour {
		layout = "02 Jan 15:04"
	}
	v.StartLabel, v.EndLabel = w.Start.UTC().Format(layout), w.End.UTC().Format(layout)
	if v.Total == 0 {
		return v
	}
	v.ErrPct = float64(v.Errors) / float64(v.Total) * 100
	switch {
	case v.Errors > 0 && v.ErrPct < 0.1:
		v.ErrPctText = "<0.1%"
	default:
		v.ErrPctText = fmt.Sprintf("%.1f%%", v.ErrPct)
	}
	var peak int64
	for _, cn := range by {
		peak = max(peak, cn.info+cn.warn+cn.err)
	}
	v.MaxText = compactCount(peak)
	span := float64(w.End.Sub(w.Start) / time.Second)
	slot := float64(bucket/time.Second) / span * volW
	bw := fmax(slot*0.8, 1.5)
	times := make([]int64, 0, len(by))
	for t := range by {
		times = append(times, t)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	// Record volume is long-tailed: one burst can be a thousand times a normal
	// interval and flatten every other bar. Column height follows the square
	// root of the total so ordinary intervals stay visible, and the segments
	// split that height in proportion to their counts. The chart says so.
	root := math.Sqrt(float64(peak))
	for _, t := range times {
		cn := by[t]
		total := cn.info + cn.warn + cn.err
		x := fmin(fmax((float64(t)-float64(w.Start.Unix()))/span*volW, 0), volW-bw)
		b := obsBar{X: x, W: bw, HasInfo: cn.info > 0, HasWarn: cn.warn > 0, HasEr: cn.err > 0}
		colH := fmax(math.Sqrt(float64(total))/root*(volH-4), 3)
		base := volH
		seg := func(n int64) (y, h float64) {
			h = fmax(colH*float64(n)/float64(total), 1.2)
			base -= h
			return base, h
		}
		if cn.info > 0 {
			b.InfoY, b.InfoH = seg(cn.info)
		}
		if cn.warn > 0 {
			b.WarnY, b.WarnH = seg(cn.warn)
		}
		if cn.err > 0 {
			b.ErrY, b.ErrH = seg(cn.err)
		}
		b.Title = fmt.Sprintf("%s UTC · %d info · %d warn · %d error", time.Unix(t, 0).UTC().Format("15:04"), cn.info, cn.warn, cn.err)
		v.Bars = append(v.Bars, b)
	}
	return v
}

// compactCount renders 12345 as 12.3k.
func compactCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}
