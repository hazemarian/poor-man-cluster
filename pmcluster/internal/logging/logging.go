// Package logging owns pmcluster's log-level vocabulary and the zerolog
// setup both binaries start with.
//
// One env var, PMCLUSTER_LOG_LEVEL, sets the verbosity of the whole process
// (the daemon/CLI and the edge), and it is the env side of the `log_level`
// cluster setting: `cluster update` renders that setting into the platform
// stack as PMCLUSTER_LOG_LEVEL, so a container and a host process that were
// configured together also log at the same level.
//
// The accepted names are also the ones the DSL and the cluster settings
// accept (debug|info|warn|error, with `warning` as an alias of warn), so a
// value validated in one place is never rejected in another.
package logging

import (
	"io"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

const (
	// EnvVar is the process-level knob. It is set by hand on a dev host and
	// rendered into the platform stack from the `log_level` cluster setting.
	EnvVar = "PMCLUSTER_LOG_LEVEL"

	// DefaultLevel is the daemon's level when EnvVar is unset or names an
	// unknown level.
	DefaultLevel = "info"

	// DefaultAppLevel is the level injected into deployed app services when
	// neither the manifest's `log_level` nor the `app_log_level` cluster
	// setting names one. Apps are chatty at INFO; warn is the quiet default.
	DefaultAppLevel = "warn"

	// AllowedNames is the level list as one string, for error messages that
	// must tell the operator what to type instead.
	AllowedNames = "debug|info|warn|error"
)

// Normalize returns the canonical spelling of a level: lowercased, trimmed,
// with the `warning` alias mapped onto `warn`. An unknown or empty value
// returns "" — callers decide whether that is an error or a fallback.
func Normalize(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return "debug"
	case "info":
		return "info"
	case "warn", "warning":
		return "warn"
	case "error":
		return "error"
	default:
		return ""
	}
}

// LevelOrDefault returns the canonical level, or def when level is empty or
// unknown. An unknown def falls back to DefaultLevel, so this never returns
// something zerolog would silently treat as info.
func LevelOrDefault(level, def string) string {
	if n := Normalize(level); n != "" {
		return n
	}
	if n := Normalize(def); n != "" {
		return n
	}
	return DefaultLevel
}

// Parse maps a level name to its zerolog level; ok is false when the value is
// not a level this platform accepts.
func Parse(level string) (zerolog.Level, bool) {
	switch Normalize(level) {
	case "debug":
		return zerolog.DebugLevel, true
	case "info":
		return zerolog.InfoLevel, true
	case "warn":
		return zerolog.WarnLevel, true
	case "error":
		return zerolog.ErrorLevel, true
	default:
		return zerolog.InfoLevel, false
	}
}

// Configure returns a console logger at the given level and makes that level
// the process-wide floor (zerolog.SetGlobalLevel), so loggers created
// elsewhere in the process — including the daemon's file + OTel sinks — are
// filtered by the same decision.
//
// Lines go to stderr with an RFC3339 timestamp and the level, so a line is
// attributable without a second lookup. An unknown level means DefaultLevel:
// this runs at startup and must never be the reason a binary fails to start.
func Configure(level string) zerolog.Logger {
	return configure(LevelOrDefault(level, DefaultLevel), os.Stderr)
}

// ConfigureFromEnv configures from PMCLUSTER_LOG_LEVEL. An unset value means
// fallback (or DefaultLevel); an unknown value means fallback AND one warning
// line naming the rejected value, because a typo in a systemd unit or a
// compose file is otherwise invisible: the process logs at the wrong level and
// says nothing about it.
func ConfigureFromEnv(fallback string) zerolog.Logger {
	return configureFromEnv(fallback, os.Stderr)
}

// configureFromEnv is ConfigureFromEnv with an explicit sink, so the warning
// line can be asserted instead of read off a terminal.
func configureFromEnv(fallback string, out io.Writer) zerolog.Logger {
	fallback = LevelOrDefault(fallback, DefaultLevel)

	raw := strings.TrimSpace(os.Getenv(EnvVar))
	name := Normalize(raw)
	if raw != "" && name == "" {
		log := configure(fallback, out)
		log.Warn().
			Str("env", EnvVar).
			Str("value", raw).
			Str("using", fallback).
			Msg("unknown log level — allowed: " + AllowedNames)
		return log
	}
	if name == "" {
		name = fallback
	}
	return configure(name, out)
}

// configure builds the console logger and sets the process-wide level.
func configure(name string, out io.Writer) zerolog.Logger {
	if out == nil {
		out = os.Stderr
	}
	lvl, _ := Parse(name)
	zerolog.SetGlobalLevel(lvl)

	return zerolog.New(zerolog.ConsoleWriter{Out: out, TimeFormat: time.RFC3339}).
		Level(lvl).
		With().Timestamp().Logger()
}
