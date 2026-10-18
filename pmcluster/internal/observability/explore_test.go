package observability

import (
	"strings"
	"testing"
	"time"
)

func TestCleanSearchStripsControlAndCaps(t *testing.T) {
	got := CleanSearch("  a\x00b\nc" + strings.Repeat("x", 200))
	if strings.ContainsAny(got, "\x00\n") || len([]rune(got)) != MaxSearchRunes {
		t.Fatalf("%q", got)
	}
}

func TestBucketForKeepsSeriesShort(t *testing.T) {
	end := time.Now()
	for span, want := range map[time.Duration]time.Duration{15 * time.Minute: 30 * time.Second, time.Hour: time.Minute, 6 * time.Hour: 5 * time.Minute, 24 * time.Hour: 15 * time.Minute} {
		w := Window{Start: end.Add(-span), End: end}
		if got := BucketFor(w); got != want {
			t.Errorf("%s: %s", span, got)
		}
		if err := (MetricQuery{Window: w, Bucket: BucketFor(w)}).Validate(); err != nil {
			t.Errorf("%s: %v", span, err)
		}
	}
}

func TestValidSeverityIsAllowlist(t *testing.T) {
	for _, ok := range []string{"", "error", "warn", "info", "debug"} {
		if !ValidSeverity(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"ERROR", "fatal", "error' --"} {
		if ValidSeverity(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
