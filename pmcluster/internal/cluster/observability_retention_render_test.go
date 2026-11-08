package cluster

import (
	"context"
	"regexp"
	"strconv"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestObservabilityStackCarriesConfiguredRetention is the "the setting actually
// reaches the script" test, end to end through the render path: a value written
// to the store flows through loadObservabilityRetention into the observability
// stack's provision one-shot env, which is the ONLY way the provision script
// learns the day count (it has no store and no API access). The assertions are
// on the parsed env map, so a rename in the template fails here rather than in
// the cluster.
func TestObservabilityStackCarriesConfiguredRetention(t *testing.T) {
	st, _ := newTestDeps(t)
	ctx := context.Background()

	// Non-default values for every stream: an off-by-one or a swapped pair
	// would otherwise be invisible because 30/14/10 are also the fallbacks.
	for key, value := range map[string]string{
		settingObservabilityLogsRetentionDays:    "21",
		settingObservabilityMetricsRetentionDays: "9",
		settingObservabilityTracesRetentionDays:  "3",
	} {
		if err := st.SetSetting(ctx, key, value); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
	}

	logs, metrics, traces := loadObservabilityRetention(ctx, st)
	if logs != 21 || metrics != 9 || traces != 3 {
		t.Fatalf("loadObservabilityRetention = (%d, %d, %d), want (21, 9, 3)", logs, metrics, traces)
	}

	data, err := LoadComposeFile(StackObservability, RenderInput{
		Domain:                            "example.com",
		ObservabilityAdminEmail:           "ops@example.com",
		ObservabilityAdminPassword:        "SOMEPASSWORD_SECRET",
		ObservabilityBasicAuth:            observabilityBasicAuth("ops@example.com", "SOMEPASSWORD_SECRET"),
		ObservabilityLogsRetentionDays:    logs,
		ObservabilityMetricsRetentionDays: metrics,
		ObservabilityTracesRetentionDays:  traces,
	})
	if err != nil {
		t.Fatalf("LoadComposeFile: %v", err)
	}

	var doc struct {
		Services map[string]struct {
			// sigs.k8s.io/yaml routes through encoding/json, so these are json
			// tags. The renderer normalises the template's `env:` key to
			// Compose's canonical `environment:`.
			Env map[string]string `json:"environment"`
		} `json:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse rendered observability stack: %v", err)
	}
	provision, ok := doc.Services["provision"]
	if !ok {
		t.Fatalf("rendered stack has no provision one-shot (services: %v)", keysOf(doc.Services))
	}
	for name, want := range map[string]string{
		"LOGS_RETENTION_DAYS":    "21",
		"METRICS_RETENTION_DAYS": "9",
		"TRACES_RETENTION_DAYS":  "3",
	} {
		if got := provision.Env[name]; got != want {
			t.Errorf("provision env %s = %q, want %q (the stored setting must reach the script)", name, got, want)
		}
	}
}

// TestObservabilityProvisionScriptDefaultsMatchSettingsDefaults keeps the two
// places a default can live in step: the script's `${VAR:-N}` fallback (used
// when the env var is missing at the one-shot) and the value `settings Get`
// reports as in effect. If they drift, an untouched cluster reports 30 days
// while the script applies something else — silently.
func TestObservabilityProvisionScriptDefaultsMatchSettingsDefaults(t *testing.T) {
	body, err := RenderObservabilityProvisionScript(RenderInput{})
	if err != nil {
		t.Fatalf("render provision script: %v", err)
	}

	want := map[string]int{
		"LOGS_RETENTION_DAYS":    DefaultObservabilityLogsRetentionDays(),
		"METRICS_RETENTION_DAYS": DefaultObservabilityMetricsRetentionDays(),
		"TRACES_RETENTION_DAYS":  DefaultObservabilityTracesRetentionDays(),
	}
	for variable, def := range want {
		pattern := regexp.MustCompile(`\$\{` + variable + `:-([0-9]*)\}`)
		match := pattern.FindStringSubmatch(string(body))
		if match == nil {
			t.Errorf("provision script has no ${%s:-<default>} fallback", variable)
			continue
		}
		got, err := strconv.Atoi(match[1])
		if err != nil {
			t.Errorf("%s fallback %q is not a number", variable, match[1])
			continue
		}
		if got != def {
			t.Errorf("script fallback for %s is %d, but the platform default is %d", variable, got, def)
		}
		if got <= 0 {
			t.Errorf("script fallback for %s is %d; an unset retention must never render unbounded", variable, got)
		}
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
