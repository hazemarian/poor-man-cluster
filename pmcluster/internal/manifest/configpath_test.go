package manifest

import (
	"context"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/refs"
)

// The mount location is the platform's decision and follows one convention, so a
// stack never carries a container path.
func TestConfigMountPathConvention(t *testing.T) {
	cases := map[string]string{
		"plugins":        "/etc/config/plugins.yaml",
		"app.config":     "/etc/config/app.config",
		"nginx.conf":     "/etc/config/nginx.conf",
		"  spaced  ":     "/etc/config/spaced.yaml",
		"/leading-slash": "/etc/config/leading-slash.yaml",
	}
	for name, want := range cases {
		if got := refs.DefaultConfigMountPath(name); got != want {
			t.Errorf("DefaultConfigMountPath(%q) = %q, want %q", name, got, want)
		}
	}
	// A relocated root is honoured, and emptying it falls back to the default.
	if got := refs.ConfigMountPath("/srv/conf", "plugins"); got != "/srv/conf/plugins.yaml" {
		t.Errorf("relocated root = %q", got)
	}
	if got := refs.ConfigMountPath("", "plugins"); got != "/etc/config/plugins.yaml" {
		t.Errorf("empty root should fall back to the default, got %q", got)
	}
}

// A service that mounts configs is handed the directory by the platform; a
// service that mounts none must not be, and an explicit value always wins.
func TestConfigPathEnvIsInjectedOnlyWithConfigs(t *testing.T) {
	manifest := `
app: cfgapp
env: production
domain: example.test
version: v1
services:
  with_configs:
    image: nginx:1.27
    configs:
      - config_path(plugins)
  without_configs:
    image: nginx:1.27
  explicit:
    image: nginx:1.27
    configs:
      - config_path(plugins)
    env:
      CONFIG_PATH: /custom/place
`
	app, ir, err := ParseBuild(context.Background(), []byte(manifest), "", nil)
	if err != nil {
		t.Fatalf("ParseBuild: %v", err)
	}
	_ = app
	byName := map[string]map[string]string{}
	for _, svc := range ir.Services {
		byName[svc.Name] = svc.Env
	}
	if got := byName["with_configs"]["CONFIG_PATH"]; got != refs.DefaultConfigMountRoot {
		t.Errorf("CONFIG_PATH = %q, want %q", got, refs.DefaultConfigMountRoot)
	}
	if _, set := byName["without_configs"]["CONFIG_PATH"]; set {
		t.Errorf("CONFIG_PATH injected into a service with no configs")
	}
	if got := byName["explicit"]["CONFIG_PATH"]; got != "/custom/place" {
		t.Errorf("manifest value was overridden: %q", got)
	}

	// The mount target and the injected directory must describe the same tree.
	for _, svc := range ir.Services {
		if svc.Name != "with_configs" {
			continue
		}
		if len(svc.Configs) != 1 {
			t.Fatalf("configs = %d, want 1", len(svc.Configs))
		}
		target := svc.Configs[0].Target
		if !strings.HasPrefix(target, refs.DefaultConfigMountRoot+"/") {
			t.Errorf("mount target %q is outside the CONFIG_PATH directory %q", target, refs.DefaultConfigMountRoot)
		}
	}
}

// `env: VAR: config_path(name)` resolves to the SAME path the mount uses.
func TestEnvConfigPathRefMatchesMountTarget(t *testing.T) {
	manifest := `
app: cfgapp
env: production
domain: example.test
version: v1
services:
  web:
    image: nginx:1.27
    configs:
      - config_path(plugins)
    env:
      PLUGINS_CONFIG_PATH: config_path(plugins)
`
	_, ir, err := ParseBuild(context.Background(), []byte(manifest), "", nil)
	if err != nil {
		t.Fatalf("ParseBuild: %v", err)
	}
	var mountTarget, envValue string
	for _, svc := range ir.Services {
		envValue = svc.Env["PLUGINS_CONFIG_PATH"]
		if len(svc.Configs) > 0 {
			mountTarget = svc.Configs[0].Target
		}
	}
	if mountTarget == "" || envValue == "" {
		t.Fatalf("missing mount (%q) or env (%q)", mountTarget, envValue)
	}
	if mountTarget != envValue {
		t.Errorf("env %q and mount %q disagree — they must be one decision", envValue, mountTarget)
	}
}
