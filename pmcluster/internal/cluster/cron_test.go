package cluster

import (
	"testing"
	"time"
)

func TestCronDueMatchesTheMinuteItShould(t *testing.T) {
	// 2026-11-04 is a Wednesday; 02:30 local.
	at := func(h, m int) time.Time { return time.Date(2026, 11, 4, h, m, 0, 0, time.Local) }

	cases := []struct {
		expr string
		when time.Time
		want bool
	}{
		{"0 * * * *", at(2, 0), true},
		{"0 * * * *", at(2, 30), false},
		{"*/1 * * * *", at(2, 30), false}, // steps are refused, not ignored
		{"0 2 * * *", at(2, 0), true},
		{"0 2 * * *", at(3, 0), false},
		{"30 1,13 * * *", at(13, 30), true},
		{"30 1,13 * * *", at(13, 0), false},
		{"0 0 4 11 *", at(0, 0), true}, // day+month match today
		{"* * * * *", at(9, 17), true},
	}
	for _, c := range cases {
		got, err := CronDue(c.expr, c.when)
		if c.expr == "*/1 * * * *" {
			if err == nil {
				t.Fatalf("%s: a step expression must be refused, not silently unmatched", c.expr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got != c.want {
			t.Fatalf("%s at %s: got %v, want %v", c.expr, c.when.Format("15:04"), got, c.want)
		}
	}
}

func TestCronDueRejectsMalformedExpressions(t *testing.T) {
	for _, expr := range []string{"", "0 * * *", "every hour", "0 25 * * *", "0 * * * 9"} {
		if _, err := CronDue(expr, time.Now()); err == nil {
			t.Fatalf("%q should have been rejected", expr)
		}
	}
}
