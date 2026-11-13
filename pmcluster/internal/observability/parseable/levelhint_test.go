package parseable

import (
	"regexp"
	"strings"
	"testing"
)

// patternOf pulls the regular expression out of a `regexp_like(..., '<pattern>')` fragment
// so it can be exercised by Go's own engine. The two dialects agree on everything these
// patterns use (inline (?i), groups, alternation, \t, character classes), which is what
// makes this worth doing: the bug below was a pattern that silently matched nothing, and a
// string comparison would have "passed" it again.
func patternOf(t *testing.T, sql string) *regexp.Regexp {
	t.Helper()
	const open = ", '"
	const close = "')"
	i := strings.Index(sql, open)
	j := strings.LastIndex(sql, close)
	if i < 0 || j <= i {
		t.Fatalf("cannot extract a pattern from %q", sql)
	}
	re, err := regexp.Compile(sql[i+len(open) : j])
	if err != nil {
		t.Fatalf("pattern does not compile: %v", err)
	}
	return re
}

// TestLevelHintsRecogniseZerologJSON is the regression test for a log panel that lied.
//
// Container output has no level of its own, so the collector labels stderr ERROR; the panel
// then recovers the real level from the text. The hint patterns knew logfmt (`level=info`)
// and several prefix formats, but not the one this platform actually writes — zerolog's
// JSON, `"level":"info"`. The quotes broke the match, every daemon log line fell through to
// "error", and a quiet cluster rendered as an error firehose in its own explore panel.
//
// The console abbreviation (`INF`) was missing for the same reason: it is what zerolog's
// ConsoleWriter emits, and it matches no other alternative.
func TestLevelHintsRecogniseZerologJSON(t *testing.T) {
	info := patternOf(t, infoHintSQL)
	warn := patternOf(t, warnHintSQL)

	infoSamples := []string{
		`{"level":"info","time":"2026-11-13T13:10:00Z","message":"reconcile pass"}`,
		`{"level":"debug","message":"cache hit"}`,
		`13:10:00 INF reconcile pass`,
		`13:10:00 DBG cache hit`,
		`13:10:00 TRC detail`,
		`time=2026-11-13T13:10:00Z level=info msg="started"`,
		"\tinfo\tstarting",
	}
	for _, s := range infoSamples {
		if !info.MatchString(s) {
			t.Errorf("info pattern does not match %q — it will be counted as an error", s)
		}
	}

	warnSamples := []string{
		`{"level":"warn","message":"slow"}`,
		`{"level":"warning","message":"slow"}`,
		`13:10:00 WRN slow`,
		`level=warn msg=slow`,
	}
	for _, s := range warnSamples {
		if !warn.MatchString(s) {
			t.Errorf("warn pattern does not match %q", s)
		}
	}

	// The alternatives must not be greedy enough to swallow an unrelated word: `INF` is
	// space-delimited on purpose, so a buffer containing GOLANGINF is not "info".
	if info.MatchString(`{"level":"error","message":"GOLANGINFO"}`) {
		t.Error("info pattern matched a line whose only resemblance is a substring")
	}
	// And an ERROR record must not be reclassified by either hint.
	if info.MatchString(`{"level":"error","message":"failed"}`) || warn.MatchString(`{"level":"error","message":"failed"}`) {
		t.Error("an error-level record was matched by an info or warn hint")
	}
}
