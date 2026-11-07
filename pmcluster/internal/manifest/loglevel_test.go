package manifest

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-cluster/pmcluster/pkg/dsl"
)

// renderedService is the subset of a rendered compose service these tests
// read. LoadComposeFile rewrites `env:` to `environment:`, so both tags are
// needed — the writer emits `environment:`.
type renderedService struct {
	Environment map[string]string `json:"environment"`
	Env         map[string]string `json:"env"`
}

func (s renderedService) env() map[string]string {
	if s.Environment != nil {
		return s.Environment
	}
	return s.Env
}

// renderServices translates an app and returns the rendered services by name.
func renderServices(t *testing.T, app *dsl.App) map[string]renderedService {
	t.Helper()
	out, err := TranslateIR(context.Background(), app, nil, nil)
	if err != nil {
		t.Fatalf("TranslateIR: %v", err)
	}
	var doc struct {
		Services map[string]renderedService `json:"services"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal rendered compose: %v\n%s", err, out)
	}
	return doc.Services
}

// TestLogLevelInjectedIntoEveryService pins the default: a manifest that says
// nothing about logging still gets LOG_LEVEL=warn on EVERY service, so a
// chatty app stops shouting at INFO without editing each manifest.
func TestLogLevelInjectedIntoEveryService(t *testing.T) {
	app := baseApp()
	app.Services = map[string]*dsl.Service{
		"api":    {Image: "nginx:latest"},
		"worker": {Image: "busybox:latest"},
	}

	for name, svc := range renderServices(t, app) {
		if got := svc.env()["LOG_LEVEL"]; got != "warn" {
			t.Errorf("services.%s LOG_LEVEL = %q, want warn (the built-in default)", name, got)
		}
	}
}

// TestLogLevelPrecedence is the whole contract in one test: the manifest's
// log_level reaches every service, EXCEPT a service that declares LOG_LEVEL
// itself — an explicit value in the app's own env always wins.
func TestLogLevelPrecedence(t *testing.T) {
	app := baseApp()
	app.LogLevel = "debug"
	app.Services = map[string]*dsl.Service{
		"api": {
			Image: "nginx:latest",
			Env:   map[string]string{"LOG_LEVEL": "error"},
		},
		"worker": {Image: "busybox:latest"},
	}

	svcs := renderServices(t, app)
	if got := svcs["api"].env()["LOG_LEVEL"]; got != "error" {
		t.Errorf("services.api LOG_LEVEL = %q, want error (the app's own value wins)", got)
	}
	if got := svcs["worker"].env()["LOG_LEVEL"]; got != "debug" {
		t.Errorf("services.worker LOG_LEVEL = %q, want debug (the manifest's log_level)", got)
	}
}

// TestLogLevelWarningAliasRendersWarn: `warning` is accepted, and the rendered
// value is the canonical `warn` an app's log library expects.
func TestLogLevelWarningAliasRendersWarn(t *testing.T) {
	app := baseApp()
	app.LogLevel = "warning"

	if got := renderServices(t, app)["api"].env()["LOG_LEVEL"]; got != "warn" {
		t.Errorf("LOG_LEVEL = %q, want warn (normalized from the warning alias)", got)
	}
}

// TestLogLevelIRValueWinsOverWriterDefault: the deploy service resolves the
// cluster's app_log_level onto the IR when the manifest sets none, and the
// writer must render that value rather than its own default.
func TestLogLevelIRValueWinsOverWriterDefault(t *testing.T) {
	app := baseApp()
	ir, err := BuildIR(context.Background(), app, nil)
	if err != nil {
		t.Fatalf("BuildIR: %v", err)
	}
	if ir.LogLevel != "" {
		t.Fatalf("IR.LogLevel = %q, want empty when the manifest sets none", ir.LogLevel)
	}
	ir.LogLevel = "info" // what stacks.Service does from app_log_level

	out, err := (&ComposeWriter{}).Write(context.Background(), ir)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(string(out), "LOG_LEVEL: info") {
		t.Errorf("rendered compose should carry the IR's level:\n%s", out)
	}
}

// TestLogLevelNotInjectedIntoPlatformStacks: platform components (Traefik,
// Parseable, SeaweedFS) set verbosity their own way; stamping LOG_LEVEL on
// them would suggest a knob they do not read.
func TestLogLevelNotInjectedIntoPlatformStacks(t *testing.T) {
	app := baseApp()
	app.Platform = true
	app.LogLevel = "debug"

	if _, set := renderServices(t, app)["api"].env()["LOG_LEVEL"]; set {
		t.Errorf("platform stack received LOG_LEVEL; platform services set their own verbosity")
	}
}

// TestSubsetKeepsLogLevel guards the multi-level deploy path: applying a
// depends_on level renders ir.Subset(level), and a lost LogLevel there would
// produce a different LOG_LEVEL per level — a rendered-hash that never matches
// and a redeploy loop.
func TestSubsetKeepsLogLevel(t *testing.T) {
	app := baseApp()
	app.LogLevel = "error"
	ir, err := BuildIR(context.Background(), app, nil)
	if err != nil {
		t.Fatalf("BuildIR: %v", err)
	}
	if got := ir.Subset([]string{"api"}).LogLevel; got != "error" {
		t.Errorf("subset LogLevel = %q, want error", got)
	}
}

func TestValidateRejectsUnknownLogLevel(t *testing.T) {
	app := baseApp()
	app.LogLevel = "verbose"
	mustFail(t, app, "log_level: must be one of debug|info|warn|error")
}
