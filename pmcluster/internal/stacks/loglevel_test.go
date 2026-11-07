package stacks

import (
	"context"
	"testing"

	"sigs.k8s.io/yaml"
)

// logLevelManifest is a two-service app whose worker already declares its own
// LOG_LEVEL — the precedence case the injection must respect.
const logLevelManifest = `app: log-demo
env: production
domain: demo.example.com
services:
  api:
    image: nginx:latest
  worker:
    image: busybox:latest
    env:
      LOG_LEVEL: error
`

// renderedEnv returns the rendered env of each service in a stored revision.
func renderedEnv(t *testing.T, yamlDoc string) map[string]map[string]string {
	t.Helper()
	var doc struct {
		Services map[string]struct {
			Environment map[string]string `json:"environment"`
			Env         map[string]string `json:"env"`
		} `json:"services"`
	}
	if err := yaml.Unmarshal([]byte(yamlDoc), &doc); err != nil {
		t.Fatalf("unmarshal rendered compose: %v", err)
	}
	out := make(map[string]map[string]string, len(doc.Services))
	for name, svc := range doc.Services {
		if svc.Environment != nil {
			out[name] = svc.Environment
			continue
		}
		out[name] = svc.Env
	}
	return out
}

// deployRendered deploys the manifest through the real pipeline and returns the
// rendered YAML recorded for that revision.
func deployRendered(t *testing.T, manifest string, appLogLevel string) string {
	t.Helper()
	s := openTestStore(t)
	svc := newService(s, &recordingDeployer{})
	svc.AppLogLevel = appLogLevel
	ctx := context.Background()

	res, err := svc.Deploy(ctx, Payload{Manifest: manifest})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	rev, err := s.Revision(ctx, "log-demo", res.Revision)
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	return rev.RenderedYAML
}

// TestDeploy_InjectsAppLogLevelSetting: the cluster's app_log_level setting
// reaches every service that does not declare LOG_LEVEL, and loses to the one
// that does.
func TestDeploy_InjectsAppLogLevelSetting(t *testing.T) {
	env := renderedEnv(t, deployRendered(t, logLevelManifest, "info"))

	if got := env["api"]["LOG_LEVEL"]; got != "info" {
		t.Errorf("api LOG_LEVEL = %q, want info (the app_log_level setting)", got)
	}
	if got := env["worker"]["LOG_LEVEL"]; got != "error" {
		t.Errorf("worker LOG_LEVEL = %q, want error (declared in the manifest, must not be overridden)", got)
	}
}

// TestDeploy_ManifestLogLevelBeatsTheSetting: an explicit log_level in the
// manifest outranks the cluster default it would otherwise inherit.
func TestDeploy_ManifestLogLevelBeatsTheSetting(t *testing.T) {
	manifest := "log_level: debug\n" + logLevelManifest
	env := renderedEnv(t, deployRendered(t, manifest, "info"))

	if got := env["api"]["LOG_LEVEL"]; got != "debug" {
		t.Errorf("api LOG_LEVEL = %q, want debug (the manifest's log_level)", got)
	}
}

// TestDeploy_DefaultsToWarnWithNoSetting: with neither a manifest log_level nor
// an app_log_level, the built-in default is warn.
func TestDeploy_DefaultsToWarnWithNoSetting(t *testing.T) {
	env := renderedEnv(t, deployRendered(t, logLevelManifest, ""))

	if got := env["api"]["LOG_LEVEL"]; got != "warn" {
		t.Errorf("api LOG_LEVEL = %q, want warn (the built-in default)", got)
	}
}
