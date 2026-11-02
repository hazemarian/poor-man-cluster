package refs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// refResolverStub serves canned artifact names for ReplaceRefs tests.
type refResolverStub struct {
	configs map[string]string
	secrets map[string]string
	err     error
}

func (r *refResolverStub) ResolveConfig(_ context.Context, name string) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	if v, ok := r.configs[name]; ok {
		return v, nil
	}
	return "", errors.New("config not found")
}

func (r *refResolverStub) ResolveSecret(_ context.Context, name string) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	if v, ok := r.secrets[name]; ok {
		return v, nil
	}
	return "", errors.New("secret not found")
}

// TestReplaceRefs_Inline verifies the scanner replaces config()/secrets()
// references wherever they appear — including as inline compose keys and
// mount sources — and leaves plain text untouched.
func TestReplaceRefs_Inline(t *testing.T) {
	r := &refResolverStub{
		configs: map[string]string{
			"pmcluster_otel_config":     "pmcluster_otel_config_v046",
			"pmcluster_traefik_dynamic": "pmcluster_traefik_dynamic_v044",
		},
		secrets: map[string]string{
			"cert": "cert_v042",
			"key":  "key_v042",
		},
	}

	in := "configs:\n  config(pmcluster_otel_config):\n    external: true\n" +
		"  - source: config(pmcluster_traefik_dynamic)\n" +
		"secrets:\n  secrets(cert):\n  - secrets(cert)\n  - secrets(key)\n"
	want := "configs:\n  pmcluster_otel_config_v046:\n    external: true\n" +
		"  - source: pmcluster_traefik_dynamic_v044\n" +
		"secrets:\n  cert_v042:\n  - cert_v042\n  - key_v042\n"

	got, err := ReplaceRefs(context.Background(), in, r)
	if err != nil {
		t.Fatalf("ReplaceRefs: %v", err)
	}
	if got != want {
		t.Errorf("ReplaceRefs mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestReplaceRefs_NoRefs verifies plain compose text passes through untouched.
func TestReplaceRefs_NoRefs(t *testing.T) {
	r := &refResolverStub{configs: map[string]string{}, secrets: map[string]string{}}
	in := "services:\n  web:\n    image: nginx\n"
	got, err := ReplaceRefs(context.Background(), in, r)
	if err != nil {
		t.Fatalf("ReplaceRefs: %v", err)
	}
	if got != in {
		t.Errorf("plain text changed: %q", got)
	}
}

// TestReplaceRefs_ResolverError propagates resolver failures with context.
func TestReplaceRefs_ResolverError(t *testing.T) {
	r := &refResolverStub{configs: map[string]string{}, secrets: map[string]string{}, err: errors.New("boom")}
	_, err := ReplaceRefs(context.Background(), "x: config(pmcluster_otel_config)", r)
	if err == nil {
		t.Fatal("expected error from resolver")
	}
}

// TestParseEnvRef checks the whole-value env ref parser accepts/rejects the
// right shapes.
func TestParseEnvRef(t *testing.T) {
	for v, want := range map[string]bool{
		"config(my_conf)":      true,
		"secret_path(db_pass)": true,
		"config( a )":          true,
		"plain-value":          false,
		"${ENV_VAR}":           false,
		"config(":              false,
		"config()":             false,
		"prefix config(x)":     false,
		"config(x) suffix":     false,
		"secrets()":            false,
	} {
		_, ok := ParseEnvRef(v)
		if ok != want {
			t.Errorf("ParseEnvRef(%q) ok=%v, want %v", v, ok, want)
		}
	}
}

func TestMalformedEnvRef(t *testing.T) {
	for _, v := range []string{"config(", "secrets(abc", "config(,", "config())", "secrets()} x", "secrets(db_pass)", "settings(volume_root)"} {
		if !MalformedEnvRef(v) {
			t.Errorf("MalformedEnvRef(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"plain", "config(ok)", "secret_path(ok)", "CONFIG(x)"} {
		if MalformedEnvRef(v) {
			t.Errorf("MalformedEnvRef(%q) = true, want false", v)
		}
	}
}

func TestSecretMountPath(t *testing.T) {
	if got := SecretMountPath("db_pass"); got != "/run/secrets/db_pass" {
		t.Errorf("SecretMountPath = %q, want /run/secrets/db_pass", got)
	}
}

// TestReplaceRefs_ResolverErrorLeavesText verifies the offending reference is
// left untouched in the output when the resolver fails (so a compose file is
// never silently truncated mid-rewrite).
func TestReplaceRefs_ResolverErrorLeavesText(t *testing.T) {
	r := &refResolverStub{
		configs: map[string]string{"missing": ""}, // resolver returns error
		secrets: map[string]string{},
	}
	// Override: this stub returns err for unknown names. "missing" is absent.
	r.configs = map[string]string{}
	out, err := ReplaceRefs(context.Background(), "x: config(missing)", r)
	if err == nil {
		t.Fatal("expected resolver error")
	}
	if out != "x: config(missing)" {
		t.Errorf("output = %q, want original text preserved", out)
	}
}

// TestReplaceRefs_BothKindsMixedOrder verifies kind dispatch (config vs
// secrets) honors the actual reference kind, not position.
func TestReplaceRefs_BothKindsMixedOrder(t *testing.T) {
	r := &refResolverStub{
		configs: map[string]string{"c1": "cfg-1"},
		secrets: map[string]string{"s1": "sec-1"},
	}
	out, err := ReplaceRefs(context.Background(),
		"a: secrets(s1)\nb: config(c1)\nc: config(c1)", r)
	if err != nil {
		t.Fatalf("ReplaceRefs: %v", err)
	}
	want := "a: sec-1\nb: cfg-1\nc: cfg-1"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

// TestFindAll verifies extraction order, dedup, and kind separation.
func TestFindAll(t *testing.T) {
	configs, secrets := FindAll("config(b)\nconfig(a) config(b)\nsecrets(x)\nconfig(c) secrets(x)")
	if len(configs) != 3 || configs[0] != "b" || configs[1] != "a" || configs[2] != "c" {
		t.Errorf("configs = %v, want [b a c]", configs)
	}
	if len(secrets) != 1 || secrets[0] != "x" {
		t.Errorf("secrets = %v, want [x]", secrets)
	}
}

// TestFindAll_NoRefs verifies plain text yields empty result slices.
func TestFindAll_NoRefs(t *testing.T) {
	configs, secrets := FindAll("image: nginx\nports: [80]")
	if configs != nil || secrets != nil {
		t.Errorf("configs=%v secrets=%v, want nil nil", configs, secrets)
	}
}

// TestParseEnvRef_FieldExtraction verifies the parsed kind + name (with
// surrounding whitespace trimmed).
func TestParseEnvRef_FieldExtraction(t *testing.T) {
	ref, ok := ParseEnvRef("config( my_conf )")
	if !ok {
		t.Fatal("expected parse success")
	}
	if ref.Kind != "config" || ref.Name != "my_conf" {
		t.Errorf("ref = %+v, want config/my_conf", ref)
	}
}

// settingsRefResolverStub is a RefResolver that ALSO implements
// SettingsResolver — the shape the cluster-side resolver has when it can
// read cluster settings.
type settingsRefResolverStub struct {
	refResolverStub
	settings map[string]string
}

func (r *settingsRefResolverStub) ResolveSetting(_ context.Context, name string) (string, error) {
	if v, ok := r.settings[name]; ok {
		return v, nil
	}
	return "", errors.New("setting not found")
}

// TestParseSettingsRef checks settings(name) parses as a whole-value env
// ref (kind + trimmed name) and that a missing/empty close parens is flagged
// as malformed so operators fail loud at validate time.
// settings() was removed from the reference language. It must not parse as a
// reference, and it must be reported as malformed so a manifest that still uses
// it fails validation instead of shipping the literal "settings(x)" as an env
// value.
func TestParseSettingsRefIsGone(t *testing.T) {
	if ref, ok := ParseEnvRef("settings(volume_root)"); ok {
		t.Fatalf("settings() still parses as %+v", ref)
	}
	if !MalformedEnvRef("settings(volume_root)") {
		t.Error("settings() should be reported as malformed, not passed through as a literal")
	}
}

// The legacy plural spelling was removed in v0.2.225. It must not parse, and it
// must be reported malformed so a manifest still carrying `secrets(x)` fails
// validation instead of shipping that string into a container's environment.
func TestParseLegacySecretsSpellingIsRejected(t *testing.T) {
	if ref, ok := ParseEnvRef("secrets(db_pass)"); ok {
		t.Fatalf("legacy secrets() still parses as %+v", ref)
	}
	if !MalformedEnvRef("secrets(db_pass)") {
		t.Error("secrets() should be reported as malformed, not passed through as a literal")
	}
	ref, ok := ParseEnvRef("secret_path(db_pass)")
	if !ok || ref.Kind != "secret_path" || ref.Name != "db_pass" {
		t.Fatalf("secret_path ref = %+v ok=%v", ref, ok)
	}
}

// TestReplaceRefs_SettingsKind verifies settings(...) references embedded in
// compose text are rewritten through a resolver that implements
// SettingsResolver, and that a resolver WITHOUT it errors (leaving the text
// untouched) instead of silently emitting the raw reference.
func TestReplaceRefs_SettingsKind(t *testing.T) {
	in := "volumes:\n  device: settings(volume_root)/my-app\n"

	r := &settingsRefResolverStub{
		refResolverStub: refResolverStub{
			configs: map[string]string{"cfg": "cfg_v1"},
			secrets: map[string]string{"sec": "sec_v1"},
		},
		settings: map[string]string{"volume_root": "bar"},
	}
	got, err := ReplaceRefs(context.Background(), in, r)
	if err != nil {
		t.Fatalf("ReplaceRefs: %v", err)
	}
	want := "volumes:\n  device: bar/my-app\n"
	if got != want {
		t.Errorf("ReplaceRefs mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	// Mixed kinds keep dispatching correctly (config/secrets/settings).
	got, err = ReplaceRefs(context.Background(), "a: config(cfg)\nb: secrets(sec)\nc: settings(volume_root)", r)
	if err != nil {
		t.Fatalf("ReplaceRefs mixed: %v", err)
	}
	if want := "a: cfg_v1\nb: sec_v1\nc: bar"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Resolver that does NOT implement SettingsResolver → error mentioning
	// settings, with the original text preserved.
	plain := &refResolverStub{
		configs: map[string]string{},
		secrets: map[string]string{},
	}
	out, err := ReplaceRefs(context.Background(), in, plain)
	if err == nil {
		t.Fatal("expected an error from a resolver without SettingsResolver")
	}
	if !strings.Contains(err.Error(), "settings") || !strings.Contains(err.Error(), "volume_root") {
		t.Errorf("error should mention settings(volume_root): %v", err)
	}
	if out != in {
		t.Errorf("text must be preserved on error, got %q", out)
	}
}

// secretRefResolverStub resolves secret(<name>) VALUES as-is, plus the
// config/secrets kinds via the embedded stub.
type secretRefResolverStub struct {
	refResolverStub
	values map[string]string
}

func (r *secretRefResolverStub) ResolveSecretValue(_ context.Context, name string) (string, error) {
	if v, ok := r.values[name]; ok {
		return v, nil
	}
	return "", fmt.Errorf("no such secret value %q", name)
}

// TestReplaceRefs_SecretKind verifies secret(<name>) references print the
// VALUE as-is through a resolver that implements SecretValueResolver, while a
// resolver WITHOUT it errors (text preserved) instead of leaking the raw
// reference.
func TestReplaceRefs_SecretKind(t *testing.T) {
	in := "headers:\n  Authorization: \"secret(oo_basic_auth)\"\n"

	r := &secretRefResolverStub{
		refResolverStub: refResolverStub{configs: map[string]string{}, secrets: map[string]string{}},
		values:          map[string]string{"oo_basic_auth": "Basic YWRtaW46cA=="},
	}
	got, err := ReplaceRefs(context.Background(), in, r)
	if err != nil {
		t.Fatalf("ReplaceRefs: %v", err)
	}
	want := "headers:\n  Authorization: \"Basic YWRtaW46cA==\"\n"
	if got != want {
		t.Errorf("ReplaceRefs mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	// Resolver that does NOT implement SecretValueResolver → error mentioning
	// the secret name, original text preserved.
	plain := &refResolverStub{configs: map[string]string{}, secrets: map[string]string{}}
	out, err := ReplaceRefs(context.Background(), in, plain)
	if err == nil {
		t.Fatal("expected an error from a resolver without SecretValueResolver")
	}
	if !strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "oo_basic_auth") {
		t.Errorf("error should mention secret(oo_basic_auth): %v", err)
	}
	if out != in {
		t.Errorf("text must be preserved on error, got %q", out)
	}
}

// TestParseEnvRef_SecretKind verifies the envRef language now accepts
// secret(<name>) whole-value references alongside config/secrets/settings.
func TestParseEnvRef_SecretKind(t *testing.T) {
	ref, ok := ParseEnvRef("secret(oo_admin_password)")
	if !ok || ref.Kind != "secret" || ref.Name != "oo_admin_password" {
		t.Fatalf(`ParseEnvRef("secret(oo_admin_password)") = %+v, %v`, ref, ok)
	}
	if !MalformedEnvRef("secret(oo_admin_password") {
		t.Error("malformed secret ref (no close paren) must be flagged")
	}
	if MalformedEnvRef("secret(oo_admin_password)") {
		t.Error("well-formed secret ref must not be flagged malformed")
	}
}
