package manifest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/pkg/dsl"
)

// fakeResolver serves canned config content for resolver tests.
type fakeResolver struct{ values map[string]string }

func (f *fakeResolver) ResolveConfig(_ context.Context, name string) (string, error) {
	if v, ok := f.values[name]; ok {
		return v, nil
	}
	return "", errors.New("config not found")
}

func envRefApp() *dsl.App {
	return &dsl.App{
		Name:     "myapp",
		Env:      "production",
		Domain:   "example.com",
		Version:  "v1",
		Services: map[string]*dsl.Service{},
	}
}

func svcWithEnv(env map[string]string) *dsl.Service {
	return &dsl.Service{Image: "nginx", Env: env}
}

// TestEnvRef_Parse checks the ref parser accepts/rejects the right shapes.
func TestEnvRef_Parse(t *testing.T) {
	for v, want := range map[string]bool{
		"config(my_conf)":  true,
		"secrets(db_pass)": true,
		"config( a )":      true,
		"plain-value":      false,
		"${ENV_VAR}":       false,
		"config(":          false,
		"config()":         false,
		"prefix config(x)": false,
		"config(x) suffix": false,
		"secrets()":        false,
	} {
		_, ok := parseEnvRef(v)
		if ok != want {
			t.Errorf("parseEnvRef(%q) ok=%v, want %v", v, ok, want)
		}
	}
}

func TestEnvRef_Malformed(t *testing.T) {
	for _, v := range []string{"config(", "secrets(abc", "config(,", "config())", "secrets()} x"} {
		if !malformedEnvRef(v) {
			t.Errorf("malformedEnvRef(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"plain", "config(ok)", "secrets(ok)", "CONFIG(x)"} {
		if malformedEnvRef(v) {
			t.Errorf("malformedEnvRef(%q) = true, want false", v)
		}
	}
}

// TestTranslate_SecretsEnvRef verifies secrets(name) resolves to the mount
// path. The secret is NOT auto-mounted — validation requires the operator
// to list it in the service secrets: array first.
func TestTranslate_SecretsEnvRef(t *testing.T) {
	app := envRefApp()
	app.Services["web"] = svcWithEnv(map[string]string{
		"DB_PASSWORD": "secrets(db_pass)",
		"PLAIN":       "hello",
	})
	app.Services["web"].Secrets = []string{"db_pass"}

	out, err := Translate(app)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	s := string(out)

	if !strings.Contains(s, "DB_PASSWORD: /run/secrets/db_pass") {
		t.Errorf("expected DB_PASSWORD mount path in output:\n%s", s)
	}
	if !strings.Contains(s, "PLAIN: hello") {
		t.Errorf("expected plain env preserved:\n%s", s)
	}

	if !strings.Contains(s, "secrets:") || !strings.Contains(s, "external: true") {
		t.Errorf("expected external secrets block:\n%s", s)
	}
	if !strings.Contains(s, "db_pass") {
		t.Errorf("expected db_pass in service secret mounts:\n%s", s)
	}
}

// TestValidate_SecretsEnvRefNotMounted: secrets(name) in env without the
// secret listed in the service secrets: array is a validation error.
func TestValidate_SecretsEnvRefNotMounted(t *testing.T) {
	app := envRefApp()
	app.Services["web"] = svcWithEnv(map[string]string{
		"DB_PASSWORD": "secrets(db_pass)",
	})
	app.Secrets = []string{"db_pass"}

	err := Validate(app)
	if err == nil {
		t.Fatal("expected validation error for unmounted env secret")
	}
	msg := err.Error()
	if !strings.Contains(msg, "not mounted") || !strings.Contains(msg, "db_pass") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestTranslate_ConfigEnvRef verifies config(name) injects DB content.
func TestTranslate_ConfigEnvRef(t *testing.T) {
	app := envRefApp()
	app.Services["web"] = svcWithEnv(map[string]string{
		"ADMIN_ENABLED": "config(admin_flag)",
		"KEEP":          "value",
	})

	res := &fakeResolver{values: map[string]string{"admin_flag": "true"}}
	out, err := TranslateWithResolver(context.Background(), app, res)
	if err != nil {
		t.Fatalf("TranslateWithResolver: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `ADMIN_ENABLED: "true"`) && !strings.Contains(s, "ADMIN_ENABLED: true") {
		t.Errorf("expected config content injected:\n%s", s)
	}
	if !strings.Contains(s, "KEEP: value") {
		t.Errorf("expected plain env preserved:\n%s", s)
	}
}

// TestTranslate_ConfigEnvRefMissingResolver: config() without a resolver
// must fail loudly, not silently leak the raw reference into compose.
func TestTranslate_ConfigEnvRefMissingResolver(t *testing.T) {
	app := envRefApp()
	app.Services["web"] = svcWithEnv(map[string]string{"X": "config(some_cfg)"})

	_, err := Translate(app)
	if err == nil {
		t.Fatal("expected error when config() is used without a resolver")
	}
	if !strings.Contains(err.Error(), "config resolution") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestTranslate_ConfigEnvRefMissingValue: resolver returning an error must
// surface as a translate failure with the key in the message.
func TestTranslate_ConfigEnvRefMissingValue(t *testing.T) {
	app := envRefApp()
	app.Services["web"] = svcWithEnv(map[string]string{"X": "config(missing_cfg)"})

	res := &fakeResolver{values: map[string]string{}}
	_, err := TranslateWithResolver(context.Background(), app, res)
	if err == nil {
		t.Fatal("expected error for missing config")
	}
	if !strings.Contains(err.Error(), "env.X") {
		t.Errorf("error should mention env key: %v", err)
	}
}

// TestTranslate_ConfigEnvRefMultilineRejected: multi-line config content
// can't be a single compose env value.
func TestTranslate_ConfigEnvRefMultilineRejected(t *testing.T) {
	app := envRefApp()
	app.Services["web"] = svcWithEnv(map[string]string{"CFG": "config(multiline)"})

	res := &fakeResolver{values: map[string]string{"multiline": "line1\nline2"}}
	_, err := TranslateWithResolver(context.Background(), app, res)
	if err == nil {
		t.Fatal("expected error for multi-line config in env")
	}
	if !strings.Contains(err.Error(), "newlines") {
		t.Errorf("error should explain newline rule: %v", err)
	}
}

// TestValidate_MalformedEnvRef ensures Validate catches typos early.
func TestValidate_MalformedEnvRef(t *testing.T) {
	app := envRefApp()
	app.Services["web"] = svcWithEnv(map[string]string{"BAD": "config(no-closing-paren"})

	if err := Validate(app); err == nil {
		t.Fatal("expected validation error for malformed env ref")
	}
}

// TestValidate_EmptyEnvRefName: config()/secrets() with empty name rejected.
func TestValidate_EmptyEnvRefName(t *testing.T) {
	for _, v := range []string{"config()", "secrets()"} {
		app := envRefApp()
		app.Services["web"] = svcWithEnv(map[string]string{"X": v})
		if err := Validate(app); err == nil {
			t.Errorf("expected validation error for %q", v)
		}
	}
}
