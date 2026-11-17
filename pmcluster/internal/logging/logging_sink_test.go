package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestConfigureWithSink_WritesJSONToTheSinkAndFormattedTextToTheConsole is the property a container
// log pipeline depends on: the SINK must receive the zerolog JSON (an OTel bridge decodes that), while
// a human watching `docker logs` must still see the formatted line. A sink that received the console
// writer's coloured output would be undecodable — the bridge would silently drop every record.
func TestConfigureWithSink_WritesJSONToTheSinkAndFormattedTextToTheConsole(t *testing.T) {
	console := &bytes.Buffer{}
	sink := &bytes.Buffer{}

	log := configureWithSink("info", console, sink)
	log.Info().Str("stack", "aarma").Msg("deploy started")

	if sink.Len() == 0 {
		t.Fatal("the structured sink received nothing")
	}
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(sink.Bytes()), &entry); err != nil {
		t.Fatalf("the sink did not receive JSON (%q): %v", sink.String(), err)
	}
	if entry["level"] != "info" {
		t.Errorf("sink level = %v, want info", entry["level"])
	}
	if entry["message"] != "deploy started" {
		t.Errorf("sink message = %v", entry["message"])
	}
	if entry["stack"] != "aarma" {
		t.Errorf("sink lost the structured field: %v", entry["stack"])
	}

	// The console side stays human: a level and the message, with no JSON braces.
	if !strings.Contains(console.String(), "deploy started") {
		t.Errorf("the console writer saw nothing: %q", console.String())
	}
	if strings.HasPrefix(strings.TrimSpace(console.String()), "{") {
		t.Errorf("the console got raw JSON instead of formatted text: %q", console.String())
	}
}

// TestConfigureWithSink_NilSinkKeepsThePlainConsolePath guards the standalone case: with no sink the
// logger must be the ordinary console logger, not a multi-writer wrapping a nil.
func TestConfigureWithSink_NilSinkKeepsThePlainConsolePath(t *testing.T) {
	console := &bytes.Buffer{}
	log := configureWithSink("info", console, nil)
	log.Info().Msg("standalone")
	if !strings.Contains(console.String(), "standalone") {
		t.Fatalf("console = %q, want the line", console.String())
	}
	if strings.HasPrefix(strings.TrimSpace(console.String()), "{") {
		t.Errorf("no-sink logger wrote JSON: %q", console.String())
	}
}

// TestConfigureFromEnvWithSink_UnknownLevelWarnsOnlyOnce keeps the existing warning behaviour intact
// on the new path — the typo warning is what makes a bad PMCLUSTER_LOG_LEVEL visible at all.
func TestConfigureFromEnvWithSink_UnknownLevelWarnsOnlyOnce(t *testing.T) {
	t.Setenv(EnvVar, "verbose")
	console, sink := &bytes.Buffer{}, &bytes.Buffer{}
	log := configureFromEnvWithSink("info", console, sink)
	log.Info().Msg("after warning")

	if n := strings.Count(console.String(), "unknown log level"); n != 1 {
		t.Fatalf("warning lines = %d, want 1 (console: %q)", n, console.String())
	}
	if !strings.Contains(sink.String(), "after warning") {
		t.Errorf("the sink missed a later line: %q", sink.String())
	}
}
