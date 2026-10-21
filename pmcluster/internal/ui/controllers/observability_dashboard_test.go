package controllers

import (
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

func TestBuildVolumeStacksLevelsAndLeavesGapsEmpty(t *testing.T) {
	end := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	w := observability.Window{Start: end.Add(-time.Hour), End: end}
	b := func(m int) string { return end.Add(-time.Duration(m) * time.Minute).Format("2006-01-02T15:04:05") }
	rows := []observability.Row{
		{"bucket": b(50), "level": "info", "n": 90.0}, {"bucket": b(50), "level": "error", "n": 10.0},
		{"bucket": b(20), "level": "warn", "n": 5.0},
		{"bucket": "garbage", "level": "info", "n": 1.0},
	}
	v := buildVolume(rows, w, time.Minute)
	if !v.Known || v.Total != 105 || v.Errors != 10 || v.Warns != 5 || len(v.Bars) != 2 {
		t.Fatalf("%+v", v)
	}
	if v.ErrPctText != "9.5%" || v.MaxText != "100" {
		t.Errorf("%q %q", v.ErrPctText, v.MaxText)
	}
	first := v.Bars[0]
	if !first.HasInfo || !first.HasEr || first.HasWarn || first.ErrY >= first.InfoY {
		t.Errorf("error must sit above info: %+v", first)
	}
	if first.InfoY+first.InfoH > volH+0.01 || first.ErrY < 0 {
		t.Errorf("bar leaves the chart: %+v", first)
	}
	if v.StartLabel != "11:00" || v.EndLabel != "12:00" {
		t.Errorf("labels %q %q", v.StartLabel, v.EndLabel)
	}
}

func TestBuildVolumeEmptyIsKnownWithNoBars(t *testing.T) {
	end := time.Now()
	v := buildVolume(nil, observability.Window{Start: end.Add(-time.Hour), End: end}, time.Minute)
	if !v.Known || len(v.Bars) != 0 || v.Total != 0 {
		t.Fatalf("%+v", v)
	}
}

func TestTopErrorsFoldsVariantsAndIgnoresStderrInfo(t *testing.T) {
	var rows []observability.Row
	for i := 0; i < 30; i++ {
		rows = append(rows, observability.RowForLog("web", "ERROR", "db timeout after "+strings.Repeat("1", 1+i%3)+"00ms", "2026-10-18T10:00:00"))
	}
	for i := 0; i < 80; i++ {
		rows = append(rows, observability.RowForLog("collector", "ERROR", "2026-10-18T10:00:00Z\tinfo\treceiver\tstarting receiver", ""))
	}
	rows = append(rows, observability.RowForLog("api", "ERROR", "panic: nil map", ""))
	top := topErrors(rows, observability.Window{})
	if len(top) != 2 || top[0].Count != 30 || top[1].Message != "panic: nil map" {
		t.Fatalf("%+v", top)
	}
	for _, e := range top {
		if strings.Contains(e.Message, "starting receiver") {
			t.Error("stderr info line counted as an error")
		}
	}
}

func TestChartUsesGaugeWhenTooSparse(t *testing.T) {
	end := time.Now()
	w := observability.Window{Start: end.Add(-time.Hour), End: end}
	c := buildChart(map[int64]float64{end.Add(-time.Minute).Unix(): 82}, w, time.Minute, 100, "%")
	if c.Points != 1 || c.Pct != 82 || c.Level != "warn" {
		t.Errorf("%+v", c)
	}
	if c := buildChart(map[int64]float64{end.Unix(): 95}, w, time.Minute, 100, "%"); c.Level != "bad" {
		t.Errorf("level %q", c.Level)
	}
}

func TestBuildVolumeKeepsSmallIntervalsVisibleNextToABurst(t *testing.T) {
	end := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	w := observability.Window{Start: end.Add(-24 * time.Hour), End: end}
	rows := []observability.Row{
		{"bucket": end.Add(-time.Hour).Format("2006-01-02T15:04:05"), "level": "info", "n": 50000.0},
		{"bucket": end.Add(-5 * time.Hour).Format("2006-01-02T15:04:05"), "level": "info", "n": 40.0},
	}
	v := buildVolume(rows, w, 15*time.Minute)
	if len(v.Bars) != 2 {
		t.Fatalf("%+v", v.Bars)
	}
	small := v.Bars[0].InfoH
	if small < 3 || small > v.Bars[1].InfoH {
		t.Errorf("small bar %.1f vs burst %.1f", small, v.Bars[1].InfoH)
	}
	if v.StartLabel != "17 Oct 12:00" {
		t.Errorf("a 24h window needs dated labels, got %q", v.StartLabel)
	}
	if got := buildVolume([]observability.Row{{"bucket": end.Format("2006-01-02T15:04:05"), "level": "error", "n": 1.0}, {"bucket": end.Format("2006-01-02T15:04:05"), "level": "info", "n": 5000.0}}, w, 15*time.Minute).ErrPctText; got != "<0.1%" {
		t.Errorf("tiny error rate shown as %q", got)
	}
}
