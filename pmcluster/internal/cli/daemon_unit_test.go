package cli

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// unitSections parses a rendered unit into section -> key -> value, so the
// assertions below read the settings systemd would read instead of matching a
// block of text.
func unitSections(t *testing.T, unit string) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	section := ""
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[]")
			if out[section] == nil {
				out[section] = map[string]string{}
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || section == "" {
			continue
		}
		out[section][strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// unitSeconds accepts both spellings systemd accepts: a bare number of seconds
// and a duration string.
func unitSeconds(t *testing.T, key, raw string) time.Duration {
	t.Helper()
	if raw == "" {
		t.Fatalf("%s is not set in the rendered unit", key)
	}
	if n, err := strconv.Atoi(raw); err == nil {
		return time.Duration(n) * time.Second
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("%s=%q is not a number of seconds or a duration: %v", key, raw, err)
	}
	return d
}

// TestRenderSystemdUnit_BoundsTheRestartLoop pins the behaviour that keeps a
// crash loop from becoming a journal firehose. One node logged 65 Started/Stopped
// pairs in a single boot (2026-11-08) while the process was being restarted as
// fast as it could exit.
//
// The three properties: a restart backoff of at least ten seconds, a start-rate
// limit expressed inside [Unit] (where systemd reads it), and a rate no faster
// than one attempt per 30 seconds on average, so a permanent failure settles into
// `failed` instead of a loop.
func TestRenderSystemdUnit_BoundsTheRestartLoop(t *testing.T) {
	unit := renderSystemdUnit("pmuser", "docker", "/home/pmuser", "/usr/local/bin/pmcluster serve", memoryLimitsFor(2048))
	sec := unitSections(t, unit)

	svc, ok := sec["Service"]
	if !ok {
		t.Fatalf("rendered unit has no [Service] section:\n%s", unit)
	}
	if svc["Restart"] != "always" {
		t.Errorf("Restart=%q, want always (the daemon must come back on its own)", svc["Restart"])
	}
	backoff := unitSeconds(t, "RestartSec", svc["RestartSec"])
	if backoff < 10*time.Second {
		t.Errorf("RestartSec=%s, want >= 10s: a fast retry only refills the journal on a node that is already struggling", backoff)
	}

	unitSec, ok := sec["Unit"]
	if !ok {
		t.Fatalf("rendered unit has no [Unit] section:\n%s", unit)
	}
	interval := unitSeconds(t, "StartLimitIntervalSec", unitSec["StartLimitIntervalSec"])
	burst, err := strconv.Atoi(unitSec["StartLimitBurst"])
	if err != nil || burst < 1 {
		t.Fatalf("StartLimitBurst=%q, want a positive integer", unitSec["StartLimitBurst"])
	}
	if interval < time.Minute {
		t.Errorf("StartLimitIntervalSec=%s, want >= 1m: a burst window shorter than that is a normal restart, not a limit", interval)
	}
	if burst > 10 {
		t.Errorf("StartLimitBurst=%d, want <= 10 attempts per window", burst)
	}
	if slope := interval / time.Duration(burst); slope < 30*time.Second {
		t.Errorf("start-rate limit allows one attempt per %s, want >= 30s (65 starts in one boot was the incident)", slope)
	}

	// A crash loop must be able to reach `failed` at all: RestartSec alone never
	// stops, and systemd needs the limit in [Unit] to stop it.
	if unitSec["StartLimitIntervalSec"] != "" && unitSec["StartLimitBurst"] != "" && svc["Restart"] == "always" {
		return
	}
	t.Error("a bounded restart policy needs StartLimitIntervalSec, StartLimitBurst and Restart together")
}
