package manifest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/pkg/dsl"
)

// settingsResolverStub is an EnvResolver that ALSO implements
// SettingsResolver — the shape *stacks.StoreConfigResolver has in production
// (it resolves both config() and settings() refs).
type settingsResolverStub struct {
	values   map[string]string // config(name) content
	settings map[string]string // settings(name) content
	lastLook struct {
		stack string
		name  string
	}
}

func (r *settingsResolverStub) ResolveConfig(_ context.Context, _, name string) (string, error) {
	if v, ok := r.values[name]; ok {
		return v, nil
	}
	return "", errors.New("config not found")
}

func (r *settingsResolverStub) ResolveSetting(_ context.Context, stack, name string) (string, error) {
	r.lastLook.stack = stack
	r.lastLook.name = name
	if v, ok := r.settings[name]; ok {
		return v, nil
	}
	return "", errors.New("setting not found")
}

// configPathResolverStub is an EnvResolver that ALSO implements
// ConfigPathResolver — the shape *cluster.renderRefResolver has in production
// (it resolves config_path() mounts to their well-known container paths).
type configPathResolverStub struct {
	paths map[string]string
}

func (r *configPathResolverStub) ResolveConfig(_ context.Context, _, name string) (string, error) {
	return name, nil
}

func (r *configPathResolverStub) ResolveConfigPath(_ context.Context, _, name string) (string, error) {
	if p, ok := r.paths[name]; ok {
		return p, nil
	}
	return "", nil
}

// TestTranslate_SettingsEnvRef verifies an env value of settings(name) is
// substituted with the live cluster setting through a resolver that
// implements SettingsResolver, that resolvers which DON'T implement it fail
// loud with a settings-specific message (never leaking the raw reference
// into compose), and that the validate-time malformed-ref message advertises
// settings(name).
func TestTranslate_SettingsEnvRef(t *testing.T) {
	newApp := func() *dsl.App {
		app := envRefApp()
		app.Services["web"] = svcWithEnv(map[string]string{
			"VOLUME_ROOT": "settings(volume_root)",
			"KEEP":        "value",
		})
		return app
	}

	// Resolver implementing SettingsResolver → the setting value lands in
	// the rendered env (and the lookup is stack-scoped).
	res := &settingsResolverStub{
		settings: map[string]string{"volume_root": "/srv/stack/data"},
	}
	out, err := TranslateWithResolver(context.Background(), newApp(), res)
	if err != nil {
		t.Fatalf("TranslateWithResolver: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "VOLUME_ROOT: /srv/stack/data") {
		t.Errorf("expected settings(volume_root) resolved to /srv/stack/data:\n%s", s)
	}
	if strings.Contains(s, "settings(volume_root)") {
		t.Errorf("raw settings() reference must never leak into the compose:\n%s", s)
	}
	if !strings.Contains(s, "KEEP: value") {
		t.Errorf("expected plain env preserved:\n%s", s)
	}
	if res.lastLook.stack != "myapp" || res.lastLook.name != "volume_root" {
		t.Errorf("ResolveSetting called with stack=%q name=%q, want myapp/volume_root",
			res.lastLook.stack, res.lastLook.name)
	}

	// A resolver that only implements EnvResolver (no SettingsResolver) →
	// clear error mentioning settings.
	_, err = TranslateWithResolver(context.Background(), newApp(), &fakeResolver{values: map[string]string{}})
	if err == nil {
		t.Fatal("expected an error when the resolver cannot resolve settings()")
	}
	if !strings.Contains(err.Error(), "settings(volume_root)") || !strings.Contains(err.Error(), "settings resolution") {
		t.Errorf("error should name settings() and say settings resolution is unavailable: %v", err)
	}

	// No resolver at all → same loud failure, still mentioning settings.
	_, err = Translate(newApp())
	if err == nil {
		t.Fatal("expected an error when settings() is used without a resolver")
	}
	if !strings.Contains(err.Error(), "settings(volume_root)") {
		t.Errorf("error should mention the settings() reference: %v", err)
	}

	// Resolver failure (unknown setting) surfaces with the env key + ref.
	_, err = TranslateWithResolver(context.Background(), newApp(), &settingsResolverStub{})
	if err == nil {
		t.Fatal("expected an error for an unknown setting")
	}
	if !strings.Contains(err.Error(), "env.VOLUME_ROOT") || !strings.Contains(err.Error(), "resolve settings(volume_root)") {
		t.Errorf("error should name the env key and the reference: %v", err)
	}

	// Validation: a malformed settings ref fails loud and the message
	// advertises settings(name) alongside config/secrets.
	bad := envRefApp()
	bad.Services["web"] = svcWithEnv(map[string]string{"X": "settings(volume_root"})
	verr := Validate(bad)
	if verr == nil {
		t.Fatal("expected validation error for malformed settings() ref")
	}
	if !strings.Contains(verr.Error(), "settings(name)") {
		t.Errorf("malformed-ref error should mention settings(name): %v", verr)
	}
}

// secretValueResolverStub is an EnvResolver that ALSO implements
// SecretValueResolver — the shape *cluster.renderRefResolver has in
// production (it resolves secret() VALUES as-is, e.g. the OO admin password
// printed into an env var).
type secretValueResolverStub struct {
	values map[string]string
}

func (r *secretValueResolverStub) ResolveConfig(_ context.Context, _, name string) (string, error) {
	return name, nil
}

func (r *secretValueResolverStub) ResolveSecretValue(_ context.Context, _, name string) (string, error) {
	if v, ok := r.values[name]; ok {
		return v, nil
	}
	return "", errors.New("secret value not found")
}

// TestTranslate_SecretEnvRef verifies an env value of secret(name) is
// substituted with the secret VALUE as-is (the OO root password printed into
// ZO_ROOT_USER_PASSWORD), that resolvers which DON'T implement
// SecretValueResolver fail loud, and that validation advertises secret(name).
func TestTranslate_SecretEnvRef(t *testing.T) {
	newApp := func() *dsl.App {
		app := envRefApp()
		app.Services["web"] = svcWithEnv(map[string]string{
			"ZO_ROOT_USER_PASSWORD": "secret(oo_admin_password)",
			"KEEP":                  "value",
		})
		return app
	}

	res := &secretValueResolverStub{
		values: map[string]string{"oo_admin_password": "p@ss$word"},
	}
	out, err := TranslateWithResolver(context.Background(), newApp(), res)
	if err != nil {
		t.Fatalf("TranslateWithResolver: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "ZO_ROOT_USER_PASSWORD: p@ss$word") {
		t.Errorf("expected secret(oo_admin_password) printed as its value:\n%s", s)
	}
	if strings.Contains(s, "secret(oo_admin_password)") {
		t.Errorf("raw secret() reference must never leak into the compose:\n%s", s)
	}
	if !strings.Contains(s, "KEEP: value") {
		t.Errorf("expected plain env preserved:\n%s", s)
	}

	// A resolver that only implements EnvResolver → clear error mentioning
	// secret() and secret-value resolution.
	_, err = TranslateWithResolver(context.Background(), newApp(), &fakeResolver{values: map[string]string{}})
	if err == nil {
		t.Fatal("expected an error when the resolver cannot resolve secret()")
	}
	if !strings.Contains(err.Error(), "secret(oo_admin_password)") || !strings.Contains(err.Error(), "secret-value resolution") {
		t.Errorf("error should name secret() and say secret-value resolution is unavailable: %v", err)
	}

	// No resolver at all → same loud failure.
	_, err = Translate(newApp())
	if err == nil {
		t.Fatal("expected an error when secret() is used without a resolver")
	}
	if !strings.Contains(err.Error(), "secret(oo_admin_password)") {
		t.Errorf("error should mention the secret() reference: %v", err)
	}

	// Resolver failure (unknown secret) surfaces with the env key + ref.
	_, err = TranslateWithResolver(context.Background(), newApp(), &secretValueResolverStub{})
	if err == nil {
		t.Fatal("expected an error for an unknown secret value")
	}
	if !strings.Contains(err.Error(), "env.ZO_ROOT_USER_PASSWORD") || !strings.Contains(err.Error(), "resolve secret(oo_admin_password)") {
		t.Errorf("error should name the env key and the reference: %v", err)
	}

	// Validation: a malformed secret ref fails loud and the message
	// advertises secret(name).
	bad := envRefApp()
	bad.Services["web"] = svcWithEnv(map[string]string{"X": "secret(oo_admin_password"})
	verr := Validate(bad)
	if verr == nil {
		t.Fatal("expected validation error for malformed secret() ref")
	}
	if !strings.Contains(verr.Error(), "secret(name)") {
		t.Errorf("malformed-ref error should mention secret(name): %v", verr)
	}
}
