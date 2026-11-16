package cli

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// unitSections is the same helper the nodeunits tests use: it splits a rendered
// systemd unit into its sections so a test can assert what is in [Service] rather
// than grepping the whole file. Two packages, two copies — a _test.go is not
// importable, and exporting test support through production files is worse.
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
