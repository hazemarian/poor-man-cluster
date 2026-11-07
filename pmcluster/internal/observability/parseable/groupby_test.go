package parseable

import (
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

// clientForSQL builds a client whose only interesting state is the stream names
// the SQL builders interpolate. None of these tests reach the network.
func clientForSQL() *Client {
	return &Client{stream: "pmcluster-logs", metrics: "pmcluster-metrics", traces: "pmcluster-traces"}
}

func metricQueryFixture() observability.MetricQuery {
	end := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	return observability.MetricQuery{
		Window: observability.Window{Start: end.Add(-time.Hour), End: end},
		Bucket: 60 * time.Second,
	}
}

// TestGroupByNeverUsesASelectAlias is the regression that made three panels read
// "telemetry source unavailable" instead of showing an empty chart. Parseable
// runs DataFusion, which rejects `GROUP BY <alias>`:
//
//	"Column in SELECT must be in GROUP BY or an aggregate function:
//	 ... currently only pmcluster-logs.level, count(Int64(1))"
//
// Grouping by the EXPRESSION works. Every builder must therefore repeat the
// expression it selects, and this test fails the moment one goes back to the
// alias form — which is exactly what a future edit would do, because the alias
// form reads better.
func TestGroupByNeverUsesASelectAlias(t *testing.T) {
	c := clientForSQL()

	volume, err := c.LogVolumeSQL(metricQueryFixture())
	if err != nil {
		t.Fatalf("LogVolumeSQL: %v", err)
	}
	node, err := c.NodeMetricsSQL(metricQueryFixture())
	if err != nil {
		t.Fatalf("NodeMetricsSQL: %v", err)
	}
	traces, err := c.TracesSQL(observability.TraceQuery{Window: metricQueryFixture().Window, Limit: 10})
	if err != nil {
		t.Fatalf("TracesSQL: %v", err)
	}

	for _, tc := range []struct {
		name, sql string
		aliases   []string // alias forms DataFusion refuses
		mustHave  []string // the expressions that must appear instead
	}{
		{
			name:     "log volume",
			sql:      volume,
			aliases:  []string{"GROUP BY bucket, level"},
			mustHave: []string{"GROUP BY date_bin(", "CASE WHEN"},
		},
		{
			name:     "node metrics",
			sql:      node,
			aliases:  []string{"GROUP BY bucket, host, metric, state"},
			mustHave: []string{"GROUP BY date_bin(", "`host.name`", "metric_name"},
		},
		{
			name:     "traces",
			sql:      traces,
			aliases:  []string{"GROUP BY trace_id"},
			mustHave: []string{"GROUP BY span_trace_id"},
		},
	} {
		for _, bad := range tc.aliases {
			if strings.Contains(tc.sql, bad) {
				t.Errorf("%s groups by a SELECT alias (%q); DataFusion answers 400 and the panel reads as unavailable:\n%s", tc.name, bad, tc.sql)
			}
		}
		for _, want := range tc.mustHave {
			if !strings.Contains(tc.sql, want) {
				t.Errorf("%s is missing %q:\n%s", tc.name, want, tc.sql)
			}
		}
	}
}
