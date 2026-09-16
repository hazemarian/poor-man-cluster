package manifest

import (
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/pkg/dsl"
)

// minimalApp returns a *dsl.App with just enough fields to pass Interpolate
// without errors (no ${env:VAR} to resolve). Tests mutate it as needed.
func minimalApp(t *testing.T) *dsl.App {
	t.Helper()
	app, err := Parse([]byte(`
app: my-app
env: production
domain: example.com
registry: reg.example.com
version: 1.2.3
services:
  api:
    image: nginx
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return app
}

func TestInterpolate_AppBuiltin(t *testing.T) {
	app := minimalApp(t)
	app.Services["api"].Image = "${app}-image"
	if err := Interpolate(app); err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	want := "my-app-image"
	if app.Services["api"].Image != want {
		t.Errorf("image = %q, want %q", app.Services["api"].Image, want)
	}
}

func TestInterpolate_EnvBuiltin(t *testing.T) {
	app := minimalApp(t)
	app.Services["api"].Image = "img-${env}"
	if err := Interpolate(app); err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	if app.Services["api"].Image != "img-production" {
		t.Errorf("image = %q, want img-production", app.Services["api"].Image)
	}
}

func TestInterpolate_VersionBuiltin(t *testing.T) {
	app := minimalApp(t)
	app.Services["api"].Image = "img:${version}"
	if err := Interpolate(app); err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	if app.Services["api"].Image != "img:1.2.3" {
		t.Errorf("image = %q, want img:1.2.3", app.Services["api"].Image)
	}
}

func TestInterpolate_RegistryBuiltin(t *testing.T) {
	app := minimalApp(t)
	app.Services["api"].Image = "${registry}/my-app:latest"
	if err := Interpolate(app); err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	if app.Services["api"].Image != "reg.example.com/my-app:latest" {
		t.Errorf("image = %q, want reg.example.com/my-app:latest", app.Services["api"].Image)
	}
}

func TestInterpolate_DomainBuiltin(t *testing.T) {
	app := minimalApp(t)
	port := 8080
	app.Services["api"].Expose = &dsl.Expose{Port: port, Host: "api.${domain}"}
	if err := Interpolate(app); err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	if app.Services["api"].Expose.Host != "api.example.com" {
		t.Errorf("expose.host = %q, want api.example.com", app.Services["api"].Expose.Host)
	}
}

func TestInterpolate_AliasesExpandBuiltins(t *testing.T) {
	app := minimalApp(t)
	app.Services["api"].Expose = &dsl.Expose{
		Port:    8080,
		Host:    "api.${domain}",
		Aliases: []string{"idlibookfair.com", "www.${domain}"},
	}
	if err := Interpolate(app); err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	got := app.Services["api"].Expose.Aliases
	want := []string{"idlibookfair.com", "www.example.com"}
	if len(got) != len(want) {
		t.Fatalf("len(Aliases) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Aliases[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestInterpolate_DefaultVersionLatest(t *testing.T) {

	app, err := Parse([]byte(`
app: my-app
env: staging
domain: example.com
services:
  api:
    image: img:${version}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := Interpolate(app); err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	if app.Version != "latest" {
		t.Errorf("app.Version = %q, want latest", app.Version)
	}
	if app.Services["api"].Image != "img:latest" {
		t.Errorf("image = %q, want img:latest", app.Services["api"].Image)
	}
}

func TestInterpolate_EnvVarHappyPath(t *testing.T) {
	t.Setenv("MY_DOMAIN", "custom.example.com")
	app := minimalApp(t)
	app.Services["api"].Image = "img"
	app.Domain = "${env:MY_DOMAIN}"
	if err := Interpolate(app); err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	if app.Domain != "custom.example.com" {
		t.Errorf("domain = %q, want custom.example.com", app.Domain)
	}
}

func TestInterpolate_EnvVarMissing(t *testing.T) {
	app := minimalApp(t)
	app.Services["api"].Image = "${env:DEFINITELY_NOT_SET_XYZ}"
	err := Interpolate(app)
	if err == nil {
		t.Fatal("expected error for missing env var, got nil")
	}
	if !strings.Contains(err.Error(), "DEFINITELY_NOT_SET_XYZ") {
		t.Errorf("error should mention the var name, got: %v", err)
	}
}

func TestInterpolate_MultipleEnvVars(t *testing.T) {
	t.Setenv("REGISTRY_HOST", "ghcr.io")
	t.Setenv("IMAGE_TAG", "v3")
	app := minimalApp(t)
	app.Services["api"].Image = "${env:REGISTRY_HOST}/myapp:${env:IMAGE_TAG}"
	if err := Interpolate(app); err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	want := "ghcr.io/myapp:v3"
	if app.Services["api"].Image != want {
		t.Errorf("image = %q, want %q", app.Services["api"].Image, want)
	}
}

func TestInterpolate_NestedFieldsServiceImage(t *testing.T) {
	app := minimalApp(t)
	app.Services["api"].Image = "${registry}/${app}:${version}"
	if err := Interpolate(app); err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	want := "reg.example.com/my-app:1.2.3"
	if app.Services["api"].Image != want {
		t.Errorf("image = %q, want %q", app.Services["api"].Image, want)
	}
}

func TestInterpolate_NestedFieldsServiceCommand(t *testing.T) {
	app := minimalApp(t)
	app.Services["api"].Command = []string{"./run", "--env=${env}", "--version=${version}"}
	if err := Interpolate(app); err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	want := []string{"./run", "--env=production", "--version=1.2.3"}
	for i, v := range app.Services["api"].Command {
		if v != want[i] {
			t.Errorf("command[%d] = %q, want %q", i, v, want[i])
		}
	}
}

func TestInterpolate_NestedFieldsServiceEnv(t *testing.T) {
	app := minimalApp(t)
	app.Services["api"].Env = map[string]string{
		"APP_NAME": "${app}",
		"ENV_NAME": "${env}",
	}
	if err := Interpolate(app); err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	if app.Services["api"].Env["APP_NAME"] != "my-app" {
		t.Errorf("env.APP_NAME = %q, want my-app", app.Services["api"].Env["APP_NAME"])
	}
	if app.Services["api"].Env["ENV_NAME"] != "production" {
		t.Errorf("env.ENV_NAME = %q, want production", app.Services["api"].Env["ENV_NAME"])
	}
}

func TestInterpolate_NestedFieldsExposeHost(t *testing.T) {
	app := minimalApp(t)
	port := 3000
	app.Services["api"].Expose = &dsl.Expose{Port: port, Host: "api.${app}.${domain}"}
	if err := Interpolate(app); err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	want := "api.my-app.example.com"
	if app.Services["api"].Expose.Host != want {
		t.Errorf("expose.host = %q, want %q", app.Services["api"].Expose.Host, want)
	}
}
