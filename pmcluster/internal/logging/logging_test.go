package logging

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestNormalizeAcceptsTheAlias(t *testing.T) {
	cases := map[string]string{
		"debug":    "debug",
		"INFO":     "info",
		" warning": "warn",
		"Warning":  "warn",
		"error":    "error",
		"":         "",
		"verbose":  "",
		"info-ish": "",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLevelOrDefaultFallsBack(t *testing.T) {
	if got := LevelOrDefault("", DefaultAppLevel); got != "warn" {
		t.Errorf("empty level = %q, want warn", got)
	}
	if got := LevelOrDefault("nonsense", DefaultLevel); got != "info" {
		t.Errorf("unknown level = %q, want info", got)
	}
	if got := LevelOrDefault("debug", DefaultLevel); got != "debug" {
		t.Errorf("explicit level = %q, want debug", got)
	}
}

// TestConfigureFromEnvWarnsOnUnknownValue pins the degrade path: an unknown
// PMCLUSTER_LOG_LEVEL must not stop the process, and must name the value it
// rejected instead of falling into info silently.
func TestConfigureFromEnvWarnsOnUnknownValue(t *testing.T) {
	t.Setenv(EnvVar, "verbose")

	var out bytes.Buffer
	log := configureFromEnv(DefaultLevel, &out)
	log.Info().Msg("after")
	got := out.String()
	if !strings.Contains(got, "unknown log level") {
		t.Errorf("no warning about the unknown level: %q", got)
	}
	if !strings.Contains(got, "verbose") {
		t.Errorf("warning did not name the rejected value: %q", got)
	}
	if zerolog.GlobalLevel() != zerolog.InfoLevel {
		t.Errorf("global level = %v, want info (the fallback)", zerolog.GlobalLevel())
	}
}

func TestConfigureFromEnvAppliesLevel(t *testing.T) {
	t.Setenv(EnvVar, "warning")
	configureFromEnv(DefaultLevel, &bytes.Buffer{})
	if zerolog.GlobalLevel() != zerolog.WarnLevel {
		t.Errorf("global level = %v, want warn", zerolog.GlobalLevel())
	}

	t.Setenv(EnvVar, "")
	configureFromEnv(DefaultLevel, &bytes.Buffer{})
	if zerolog.GlobalLevel() != zerolog.InfoLevel {
		t.Errorf("global level with unset env = %v, want info", zerolog.GlobalLevel())
	}

	t.Setenv(EnvVar, "debug")
	configureFromEnv(DefaultLevel, &bytes.Buffer{})
	if zerolog.GlobalLevel() != zerolog.DebugLevel {
		t.Errorf("global level = %v, want debug", zerolog.GlobalLevel())
	}
}

// TestConfigureFromEnvKeepsTheFallbackLevel is the edge case that matters on
// the edge container: its own LOG_LEVEL (info) must survive when
// PMCLUSTER_LOG_LEVEL is unset.
func TestConfigureFromEnvKeepsTheFallbackLevel(t *testing.T) {
	t.Setenv(EnvVar, "")
	configureFromEnv("error", &bytes.Buffer{})
	if zerolog.GlobalLevel() != zerolog.ErrorLevel {
		t.Errorf("global level = %v, want error (the fallback)", zerolog.GlobalLevel())
	}
}
