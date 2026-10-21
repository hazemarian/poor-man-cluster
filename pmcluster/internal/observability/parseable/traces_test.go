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

func traceFixture() observability.TraceQuery {
	end := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	return observability.TraceQuery{Window: observability.Window{Start: end.Add(-time.Hour), End: end}, Limit: 25}
}

func TestTracesSQLGroupsByTraceWithFilters(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://parseable.invalid"})
	q := traceFixture()
	q.Service, q.ErrorsOnly, q.Offset = "shop'web", true, 25
	sql, err := c.TracesSQL(q)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"FROM `pmcluster-traces`",
		"`service.name` = 'shop''web'",
		"GROUP BY trace_id",
		"HAVING SUM(CASE WHEN span_status_code = 2 THEN 1 ELSE 0 END) > 0",
		"ORDER BY started DESC LIMIT 26 OFFSET 25",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q in %s", want, sql)
		}
	}
}

func TestTraceRejectsMalformedIDs(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://parseable.invalid"})
	for _, id := range []string{"", "abc", strings.Repeat("g", 32), strings.Repeat("A", 32), strings.Repeat("a", 31), "' OR 1=1 --" + strings.Repeat("a", 21), strings.Repeat("a", 33)} {
		if _, err := c.TraceSQL(id); err == nil {
			t.Errorf("accepted %q", id)
		}
	}
	if _, err := c.TraceSQL(strings.Repeat("ab", 16)); err != nil {
		t.Fatal(err)
	}
}

func TestTracesRejectUnboundedInput(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://parseable.invalid"})
	for name, mutate := range map[string]func(*observability.TraceQuery){
		"limit":   func(q *observability.TraceQuery) { q.Limit = observability.MaxTracePageSize + 1 },
		"zero":    func(q *observability.TraceQuery) { q.Limit = 0 },
		"offset":  func(q *observability.TraceQuery) { q.Offset = observability.MaxTraceOffset + 1 },
		"service": func(q *observability.TraceQuery) { q.Service = strings.Repeat("a", observability.MaxSearchRunes+1) },
	} {
		q := traceFixture()
		mutate(&q)
		if _, err := c.TracesSQL(q); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTracesKeepNumbersAndReportNextPage(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"trace_id":"5b8efff798038103d269b633813fc60d","started":"2026-10-18T19:35:31.114","duration_ns":50000000.0,"spans":3,"errors":1,"root":"GET /u/a@b.com","service":"web"},{"trace_id":"x"}]`)
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, Redactor: observability.NewRedactor()})
	q := traceFixture()
	q.Limit = 1
	res, err := c.Traces(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Rows) != 1 {
		t.Fatalf("paging: %+v", res)
	}
	row := res.Rows[0]
	if _, ok := row["duration_ns"].(json.Number); !ok {
		t.Errorf("duration stringified: %#v", row["duration_ns"])
	}
	if row["trace_id"] != "5b8efff798038103d269b633813fc60d" || row["started"] != "2026-10-18T19:35:31.114" {
		t.Errorf("structured fields rewritten: %#v", row)
	}
	if strings.Contains(fmt.Sprint(row["root"]), "a@b.com") {
		t.Errorf("free text not redacted: %v", row["root"])
	}
}

func TestLogVolumeAndTraceStatsSQLAreFixedStatements(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://parseable.invalid"})
	end := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	q := observability.MetricQuery{Window: observability.Window{Start: end.Add(-time.Hour), End: end}, Bucket: time.Minute}
	vol, err := c.LogVolumeSQL(q)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"date_bin(INTERVAL '60 seconds', p_timestamp) AS bucket", "FROM `pmcluster-logs`", "GROUP BY bucket, level", "regexp_like(SUBSTR(body, 1, 200)", "THEN 'error'", "THEN 'warn'"} {
		if !strings.Contains(vol, want) {
			t.Errorf("volume: missing %q in %s", want, vol)
		}
	}
	ts, err := c.TraceStatsSQL(q)
	if err != nil || !strings.Contains(ts, "FROM `pmcluster-traces`") || !strings.Contains(ts, "COUNT(DISTINCT span_trace_id) AS traces") {
		t.Errorf("trace stats: %v %s", err, ts)
	}
	q.Bucket = time.Second
	if _, err := c.LogVolumeSQL(q); err == nil {
		t.Error("accepted a bucket below the minimum")
	}
}
