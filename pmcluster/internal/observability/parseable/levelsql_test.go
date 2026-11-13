package parseable

import (
	"strings"
	"testing"
)

// TestLevelSQLReadsSeverityAndNothingElse pins the layer this belongs to.
//
// The level is a fact the producer knows and states once, so it is parsed into severity at
// ingest (otel-collector-config.yml: one json_parser plus a logfmt fallback). It is not a
// property to be re-derived per query from the first 200 characters of a body — that path
// produced three successive bugs (logfmt only; then a regex the SQL parser could not read;
// and, before either, stdout/stderr mapped to info/error so every warning was an ERROR) and
// each one presented as "telemetry source unavailable" in the console.
//
// The assertion is negative on purpose: if a hint ever comes back, this fails and says why.
func TestLevelSQLReadsSeverityAndNothingElse(t *testing.T) {
	if strings.Contains(levelSQL, "regexp_like") {
		t.Fatalf("levelSQL guesses the level again:\n%s", levelSQL)
	}
	if strings.Contains(levelSQL, "body") {
		t.Fatalf("levelSQL inspects the body again — the level belongs to severity_text:\n%s", levelSQL)
	}
	for _, want := range []string{"severity_text", "'error'", "'warn'", "'info'"} {
		if !strings.Contains(levelSQL, want) {
			t.Fatalf("levelSQL is missing %q:\n%s", want, levelSQL)
		}
	}
	// An unknown level falls to info, never to error: "no level stated" is not a fault.
	if strings.Index(levelSQL, "ELSE 'info'") < strings.Index(levelSQL, "'error'") {
		t.Fatal("the ELSE branch must decide the unknown case, and it must be info")
	}
}
