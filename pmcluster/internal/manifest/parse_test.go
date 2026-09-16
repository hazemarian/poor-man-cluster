package manifest

import (
	"strings"
	"testing"
)

func TestParse_EmptyInput(t *testing.T) {
	_, err := Parse([]byte(""))

	if err != nil {
		t.Logf("Parse(\"\") returned err (acceptable): %v", err)
	}
}

func TestParse_PlainText(t *testing.T) {

	_, err := Parse([]byte("not yaml at all: :::"))
	if err == nil {
		t.Fatal("expected error parsing malformed YAML, got nil")
	}
}

func TestParse_TopLevelArray(t *testing.T) {

	_, err := Parse([]byte("- item1\n- item2\n"))
	if err == nil {
		t.Fatal("expected error for top-level array, got nil")
	}
}

func TestParse_UnknownKeyTopLevel(t *testing.T) {
	bad := `
app: my-app
env: production
domain: example.com
unknown_top_level: oops
services:
  api:
    image: nginx
`
	_, err := Parse([]byte(bad))
	if err == nil {
		t.Fatal("expected error for unknown top-level key, got nil")
	}
	if !strings.Contains(err.Error(), "unknown_top_level") {
		t.Errorf("error should mention unknown key 'unknown_top_level', got: %v", err)
	}
}

func TestParse_UnknownKeyAtServiceLevel(t *testing.T) {
	bad := `
app: my-app
env: production
domain: example.com
services:
  api:
    image: nginx
    repalicas: 2
`
	_, err := Parse([]byte(bad))
	if err == nil {
		t.Fatal("expected error for unknown service key, got nil")
	}

	if !strings.Contains(err.Error(), "repalicas") {
		t.Errorf("error should mention unknown key 'repalicas', got: %v", err)
	}
}

func TestParse_ValidMinimal(t *testing.T) {
	src := `
app: my-app
env: production
domain: example.com
services:
  api:
    image: nginx:latest
`
	app, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if app == nil {
		t.Fatal("Parse returned nil app")
	}
	if app.Name != "my-app" {
		t.Errorf("app.Name = %q, want %q", app.Name, "my-app")
	}
	if app.Env != "production" {
		t.Errorf("app.Env = %q, want %q", app.Env, "production")
	}
	svc, ok := app.Services["api"]
	if !ok {
		t.Fatal("missing services.api")
	}
	if svc.Image != "nginx:latest" {
		t.Errorf("services.api.image = %q, want %q", svc.Image, "nginx:latest")
	}
}
