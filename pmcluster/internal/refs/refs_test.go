package refs

import (
	"context"
	"errors"
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
		_, ok := ParseEnvRef(v)
		if ok != want {
			t.Errorf("ParseEnvRef(%q) ok=%v, want %v", v, ok, want)
		}
	}
}

func TestMalformedEnvRef(t *testing.T) {
	for _, v := range []string{"config(", "secrets(abc", "config(,", "config())", "secrets()} x"} {
		if !MalformedEnvRef(v) {
			t.Errorf("MalformedEnvRef(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"plain", "config(ok)", "secrets(ok)", "CONFIG(x)"} {
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
