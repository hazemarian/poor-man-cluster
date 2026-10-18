package parseable

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

func logFixture() observability.LogQuery {
	end := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	return observability.LogQuery{Window: observability.Window{Start: end.Add(-time.Hour), End: end}, Limit: 2}
}

func TestLogsPageSQLNewestFirstWithFilters(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://parseable.invalid"})
	q := logFixture()
	q.Service, q.Severity, q.Search, q.Offset = "demo_web", "error", `50%_off \ it's`, 40
	sql, err := c.LogsPageSQL(q)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"`service.name` = 'demo_web'",
		"regexp_like(SUBSTR(body, 1, 200)",
		") = 'error'",
		`body ILIKE '%50\%\_off \\ it''s%' ESCAPE '\'`,
		"ORDER BY p_timestamp DESC LIMIT 3 OFFSET 40",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q in %s", want, sql)
		}
	}
}

func TestLogsRejectsUnboundedOrInjectedInput(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://parseable.invalid"})
	for name, mutate := range map[string]func(*observability.LogQuery){
		"severity": func(q *observability.LogQuery) { q.Severity = "error') OR 1=1 --" },
		"offset":   func(q *observability.LogQuery) { q.Offset = observability.MaxLogOffset + 1 },
		"negative": func(q *observability.LogQuery) { q.Offset = -1 },
		"page":     func(q *observability.LogQuery) { q.Limit = observability.MaxLogPageSize + 1 },
		"search":   func(q *observability.LogQuery) { q.Search = strings.Repeat("a", observability.MaxSearchRunes+1) },
	} {
		q := logFixture()
		mutate(&q)
		if _, err := c.LogsPageSQL(q); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLogsReportsNextPageAndRedacts(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"body":"a"},{"body":"b"},{"body":"c"}]`)
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL})
	got, err := c.Logs(context.Background(), logFixture())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 2 || !got.Truncated {
		t.Fatalf("page: %+v", got)
	}
}

func TestMetricQueriesAreFixedAndBounded(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://parseable.invalid"})
	end := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	w := observability.Window{Start: end.Add(-6 * time.Hour), End: end}
	q := observability.MetricQuery{Window: w, Bucket: observability.BucketFor(w)}
	sql, err := c.NodeMetricsSQL(q)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"date_bin(INTERVAL '300 seconds', p_timestamp)", "FROM `pmcluster-metrics`", "system.cpu.utilization", "state = 'idle'", "system.memory.utilization", "state = 'used'", "LIMIT 20000"} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q in %s", want, sql)
		}
	}
	svc, err := c.ServiceMetricsSQL(q)
	if err != nil || !strings.Contains(svc, "container.memory.percent") || !strings.Contains(svc, "LIMIT 400") {
		t.Fatalf("%v %s", err, svc)
	}
	if _, err := c.NodeMetricsSQL(observability.MetricQuery{Window: w, Bucket: time.Second}); err == nil {
		t.Error("tiny bucket accepted")
	}
	if _, err := c.NodeMetricsSQL(observability.MetricQuery{Window: w, Bucket: 30 * time.Second}); err == nil {
		t.Error("too many buckets accepted")
	}
}

func TestNodeMetricsSendsWindowBounds(t *testing.T) {
	var got map[string]string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprint(w, `[{"bucket":"2026-10-17T11:00:00","host":"n1","metric":"system.cpu.load_average.1m","value":1.5}]`)
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL})
	end := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	w := observability.Window{Start: end.Add(-time.Hour), End: end}
	res, err := c.NodeMetrics(context.Background(), observability.MetricQuery{Window: w, Bucket: time.Minute})
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if got["startTime"] != "2026-10-17T11:00:00Z" || got["endTime"] != "2026-10-17T12:00:00Z" {
		t.Errorf("bounds: %v", got)
	}
}

func TestMetricRowsKeepBucketsAndNumbersUnderRedaction(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"bucket":"2026-10-18T18:22:00","host":"n1","metric":"system.cpu.load_average.1m","value":1.75}]`)
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, Redactor: observability.NewRedactor("fixture-secret")})
	end := time.Date(2026, 10, 18, 19, 0, 0, 0, time.UTC)
	w := observability.Window{Start: end.Add(-time.Hour), End: end}
	res, err := c.NodeMetrics(context.Background(), observability.MetricQuery{Window: w, Bucket: time.Minute})
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if got := res.Rows[0]["bucket"]; got != "2026-10-18T18:22:00" {
		t.Fatalf("bucket was rewritten: %v", got)
	}
	if _, isString := res.Rows[0]["value"].(string); isString {
		t.Fatalf("value was stringified: %#v", res.Rows[0]["value"])
	}
}

func TestLogsKeepTimestampsUnderRedaction(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"p_timestamp":"2026-10-18T19:07:30.310","body":"password=hunter2 ok"}]`)
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, Redactor: observability.NewRedactor("hunter2")})
	got, err := c.Logs(context.Background(), logFixture())
	if err != nil || len(got.Rows) != 1 {
		t.Fatalf("%v %+v", err, got)
	}
	if got.Rows[0]["p_timestamp"] != "2026-10-18T19:07:30.310" {
		t.Fatalf("timestamp rewritten: %v", got.Rows[0]["p_timestamp"])
	}
	if strings.Contains(fmt.Sprint(got.Rows[0]["body"]), "hunter2") {
		t.Fatal("secret not redacted")
	}
}

func TestSeverityFiltersUseTheDashboardClassification(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://parseable.invalid"})
	for sev, want := range map[string]string{
		"error": ") = 'error'", "warn": ") = 'warn'", "info": "NOT IN ('debug','trace')", "debug": "LOWER(severity_text) IN ('debug','trace')",
	} {
		q := logFixture()
		q.Severity = sev
		sql, err := c.LogsPageSQL(q)
		if err != nil || !strings.Contains(sql, want) {
			t.Errorf("%s: %v %s", sev, err, sql)
		}
	}
}
