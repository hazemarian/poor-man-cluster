package cluster

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CronDue reports whether a 5-field cron expression matches the minute
// containing now.
//
// This exists so the backup schedule lives in the cluster's settings rather than
// in a systemd timer that a user could edit. The timer is only a heartbeat: it
// fires every hour and asks the control plane whether THIS hour is due.
//
// Every field is parsed and range-checked BEFORE any of them is compared. Doing
// it in one pass is how "0 25 * * *" survives: the minute field fails to match,
// the loop returns "not due", and the impossible hour is never looked at — so a
// typo silently becomes a schedule that never fires instead of an error the
// operator can see.
func CronDue(expr string, now time.Time) (bool, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false, fmt.Errorf("cron %q: want 5 fields (minute hour day month weekday), got %d", expr, len(fields))
	}
	specs := []struct {
		name  string
		field string
		lo    int
		hi    int
		got   int
	}{
		{"minute", fields[0], 0, 59, now.Minute()},
		{"hour", fields[1], 0, 23, now.Hour()},
		{"day", fields[2], 1, 31, now.Day()},
		{"month", fields[3], 1, 12, int(now.Month())},
		{"weekday", fields[4], 0, 6, int(now.Weekday())},
	}

	// Parse everything first.
	values := make([]map[int]bool, len(specs))
	for i, sp := range specs {
		v, err := cronFieldValues(sp.field, sp.lo, sp.hi)
		if err != nil {
			return false, fmt.Errorf("cron %q: %s: %w", expr, sp.name, err)
		}
		values[i] = v
	}
	// Then compare.
	for i, sp := range specs {
		if !cronFieldMatches(values[i], sp.got) {
			return false, nil
		}
	}
	return true, nil
}

// cronFieldValues handles `*`, a bare number, and comma-separated lists of
// numbers. Steps and ranges are deliberately refused: a schedule this platform
// cannot evaluate exactly is worse than one it rejects loudly.
func cronFieldValues(field string, lo, hi int) (map[int]bool, error) {
	if field == "*" {
		return nil, nil // nil means "matches anything"
	}
	if strings.ContainsAny(field, "*-/") && field != "*" {
		return nil, fmt.Errorf("%q: only *, N and comma lists are supported", field)
	}
	out := make(map[int]bool, 4)
	for _, part := range strings.Split(field, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("%q: %q is not a number", field, part)
		}
		if n < lo || n > hi {
			return nil, fmt.Errorf("%q: %d is outside %d..%d", field, n, lo, hi)
		}
		out[n] = true
	}
	return out, nil
}

// cronFieldMatches reports whether a field matches a value, treating a nil set
// (produced by "*") as a match for everything.
func cronFieldMatches(set map[int]bool, got int) bool {
	if set == nil {
		return true
	}
	return set[got]
}
