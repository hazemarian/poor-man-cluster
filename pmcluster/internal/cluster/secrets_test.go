package cluster

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func TestEnsureSecret_CreatesWhenMissing(t *testing.T) {
	f := newFakeDocker()
	data := []byte("super-secret")

	created, err := EnsureSecret(context.Background(), f, "my-secret", data)
	if err != nil {
		t.Fatalf("EnsureSecret: %v", err)
	}
	if !created {
		t.Error("created = false, want true for a new secret")
	}
	spec, ok := f.secrets["my-secret"]
	if !ok {
		t.Fatal("secret not found in fake after creation")
	}
	if string(spec.Data) != "super-secret" {
		t.Errorf("secret data = %q, want 'super-secret'", spec.Data)
	}
}

func TestEnsureSecret_NoOpWhenPreExisting(t *testing.T) {
	f := newFakeDocker()
	f.secrets["my-secret"] = struct {
		Name   string
		Data   []byte
		Labels map[string]string
	}{Name: "my-secret", Data: []byte("original")}

	created, err := EnsureSecret(context.Background(), f, "my-secret", []byte("replacement"))
	if err != nil {
		t.Fatalf("EnsureSecret: %v", err)
	}
	if created {
		t.Error("created = true, want false for pre-existing secret")
	}

	if string(f.secrets["my-secret"].Data) != "original" {
		t.Error("EnsureSecret modified the existing secret data")
	}
}

func TestEnsureSecret_AttachesManagedLabel(t *testing.T) {
	f := newFakeDocker()

	_, err := EnsureSecret(context.Background(), f, "labeled-secret", []byte("data"))
	if err != nil {
		t.Fatalf("EnsureSecret: %v", err)
	}
	spec := f.secrets["labeled-secret"]
	if spec.Labels[pmclusterLabel] != "true" {
		t.Errorf("label %q = %q, want 'true'", pmclusterLabel, spec.Labels[pmclusterLabel])
	}
}

func TestEnsureSecretFromFile_ReadsFileContent(t *testing.T) {
	dir := t.TempDir()
	content := []byte("file-content-for-secret")
	path := filepath.Join(dir, "testfile")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	f := newFakeDocker()
	created, err := EnsureSecretFromFile(context.Background(), f, "file-secret", path)
	if err != nil {
		t.Fatalf("EnsureSecretFromFile: %v", err)
	}
	if !created {
		t.Error("created = false, want true")
	}
	spec, ok := f.secrets["file-secret"]
	if !ok {
		t.Fatal("secret not found in fake")
	}
	if string(spec.Data) != string(content) {
		t.Errorf("secret data = %q, want %q", spec.Data, content)
	}
}

func TestRandomPassword_LengthAndEntropy(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 5; i++ {
		p, err := RandomPassword()
		if err != nil {
			t.Fatalf("RandomPassword #%d: %v", i, err)
		}
		if len(p) < 30 {
			t.Errorf("password #%d length = %d, want >= 30", i, len(p))
		}
		if seen[p] {
			t.Fatal("duplicate password returned — entropy check failed")
		}
		seen[p] = true
	}
}

func TestRandomPassword_MeetsOpenObservePolicy(t *testing.T) {
	hasLower := regexp.MustCompile(`[a-z]`)
	hasUpper := regexp.MustCompile(`[A-Z]`)
	hasDigit := regexp.MustCompile(`[0-9]`)
	hasSpecial := regexp.MustCompile(`[!@#$%^&*]`)

	for i := 0; i < 10; i++ {
		p, err := RandomPassword()
		if err != nil {
			t.Fatalf("RandomPassword: %v", err)
		}
		if len(p) < 8 || len(p) > 128 {
			t.Errorf("password length %d outside [8,128]", len(p))
		}
		if !hasLower.MatchString(p) {
			t.Errorf("password %q missing lowercase letter", p)
		}
		if !hasUpper.MatchString(p) {
			t.Errorf("password %q missing uppercase letter", p)
		}
		if !hasDigit.MatchString(p) {
			t.Errorf("password %q missing digit", p)
		}
		if !hasSpecial.MatchString(p) {
			t.Errorf("password %q missing special character", p)
		}
	}
}

func TestHtpasswdLine_Format(t *testing.T) {
	line, err := HtpasswdLine("admin", "s3cr3t")
	if err != nil {
		t.Fatalf("HtpasswdLine: %v", err)
	}

	if !strings.HasSuffix(line, "\n") {
		t.Errorf("HtpasswdLine does not end with newline: %q", line)
	}

	if !strings.HasPrefix(line, "admin:") {
		t.Errorf("HtpasswdLine does not start with 'admin:': %q", line)
	}

	trimmed := strings.TrimSuffix(line, "\n")
	parts := strings.SplitN(trimmed, ":", 2)
	if len(parts) != 2 {
		t.Fatalf("expected 'user:hash', got %q", trimmed)
	}
	hash := parts[1]

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("s3cr3t")); err != nil {
		t.Errorf("bcrypt comparison failed: %v", err)
	}
}

func TestEnsureConfig_CreatesWhenMissing(t *testing.T) {
	f := newFakeDocker()
	data := []byte("otel: config")

	name, created, err := EnsureConfig(context.Background(), f, "otel_config", data, "v0.2.0")
	if err != nil {
		t.Fatalf("EnsureConfig: %v", err)
	}
	if name != "otel_config_v001" {
		t.Errorf("name = %q, want otel_config_v001", name)
	}
	if !created {
		t.Error("created = false, want true for a fresh config")
	}
	spec, ok := f.configs[name]
	if !ok {
		t.Fatalf("config %q not found in fake after creation", name)
	}
	if string(spec.Data) != "otel: config" {
		t.Errorf("config data = %q, want 'otel: config'", spec.Data)
	}

	if spec.Labels["pmcluster.version"] != "v0.2.0" {
		t.Errorf("version label = %q, want 'v0.2.0'", spec.Labels["pmcluster.version"])
	}
}

func TestEnsureConfig_CreatesNewVersionWhenPreExisting(t *testing.T) {
	f := newFakeDocker()
	f.configs["otel_config_v001"] = struct {
		Name   string
		Data   []byte
		Labels map[string]string
	}{Name: "otel_config_v001", Data: []byte("original")}

	name, created, err := EnsureConfig(context.Background(), f, "otel_config", []byte("new config data"), "v0.2.0")
	if err != nil {
		t.Fatalf("EnsureConfig: %v", err)
	}
	if name != "otel_config_v002" {
		t.Errorf("name = %q, want otel_config_v002", name)
	}
	if !created {
		t.Error("created = false, want true for a changed render")
	}
	if string(f.configs[name].Data) != "new config data" {
		t.Errorf("config data = %q, want 'new config data'", f.configs[name].Data)
	}

	if _, exists := f.configs["otel_config_v001"]; exists {
		t.Error("old config version otel_config_v001 was not removed")
	}
}

func TestEnsureConfig_AttachesManagedLabel(t *testing.T) {
	f := newFakeDocker()

	name, _, err := EnsureConfig(context.Background(), f, "labeled-config", []byte("data"), "v0.2.0")
	if err != nil {
		t.Fatalf("EnsureConfig: %v", err)
	}
	spec := f.configs[name]
	if spec.Labels[pmclusterLabel] != "true" {
		t.Errorf("label %q = %q, want 'true'", pmclusterLabel, spec.Labels[pmclusterLabel])
	}
	if spec.Labels["pmcluster.base"] != "labeled-config" {
		t.Errorf("base label = %q, want 'labeled-config'", spec.Labels["pmcluster.base"])
	}
}

func TestEnsureConfig_ReusesVersionWhenUnchanged(t *testing.T) {
	f := newFakeDocker()
	data := []byte("same data")
	if err := f.ConfigCreate(context.Background(), runtime.ConfigSpec{
		Name: "otel_config_v001",
		Data: data,
		Labels: map[string]string{
			"pmcluster.data_hash": dataHash(data),
		},
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	name, created, err := EnsureConfig(context.Background(), f, "otel_config", data, "v0.2.0")
	if err != nil {
		t.Fatalf("EnsureConfig: %v", err)
	}
	if created {
		t.Error("created = true, want false (bytes unchanged → reuse)")
	}
	if name != "otel_config_v001" {
		t.Errorf("name = %q, want otel_config_v001 (reused)", name)
	}
	if _, ok := f.configs["otel_config_v002"]; ok {
		t.Error("otel_config_v002 was created despite unchanged bytes")
	}
}

func TestEnsureConfig_MintsNewVersionOnChange(t *testing.T) {
	f := newFakeDocker()
	if err := f.ConfigCreate(context.Background(), runtime.ConfigSpec{
		Name: "otel_config_v001",
		Data: []byte("original"),
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	name, created, err := EnsureConfig(context.Background(), f, "otel_config", []byte("changed"), "v0.2.0")
	if err != nil {
		t.Fatalf("EnsureConfig: %v", err)
	}
	if !created {
		t.Error("created = false, want true (bytes changed → new version)")
	}
	if name != "otel_config_v002" {
		t.Errorf("name = %q, want otel_config_v002", name)
	}

	if _, ok := f.configs["otel_config_v001"]; ok {
		t.Error("old otel_config_v001 not GC'd after rotate")
	}
}

// Regression test for the "new SSL config on every restart" bug: Docker never
// returns secret payloads on inspect, so comparing the bytes read back from
// Docker always failed against a real daemon and minted a fresh cert_vNNN on
// every cluster up/update. The reuse check must hash the incoming bytes and
// compare against the data-hash persisted in the store (DB = source of truth).
func TestEnsureVersionedSecret_ReusesUnchangedVersion(t *testing.T) {
	f := newFakeDocker()
	s, err := store.Open(filepath.Join(t.TempDir(), "reuse.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	seed := []byte("-----BEGIN CERTIFICATE-----\nsame cert bytes\n-----END CERTIFICATE-----\n")
	if err := f.SecretCreate(context.Background(), runtime.SecretSpec{
		Name: "cert_v001",
		Data: seed,
		Labels: map[string]string{
			pmclusterLabel:        "true",
			"pmcluster.base":      "cert",
			"pmcluster.data_hash": dataHash(seed),
		},
	}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	// The data-hash recorded in the store when the version was minted.
	if err := s.SetSetting(context.Background(), secretHashKey("cert"), dataHash(seed)); err != nil {
		t.Fatalf("seed hash: %v", err)
	}

	name, created, err := EnsureVersionedSecret(context.Background(), f, s, "cert", seed)
	if err != nil {
		t.Fatalf("EnsureVersionedSecret: %v", err)
	}
	if created {
		t.Error("created = true, want false (unchanged cert → reuse)")
	}
	if name != "cert_v001" {
		t.Errorf("name = %q, want cert_v001 (reused)", name)
	}
	if _, ok := f.secrets["cert_v002"]; ok {
		t.Error("cert_v002 was created despite unchanged bytes")
	}
}

func TestEnsureVersionedSecret_MintsNewVersionOnChange(t *testing.T) {
	f := newFakeDocker()
	s, err := store.Open(filepath.Join(t.TempDir(), "mint.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	seed := []byte("old cert bytes")
	if err := f.SecretCreate(context.Background(), runtime.SecretSpec{
		Name: "cert_v001",
		Data: seed,
		Labels: map[string]string{
			pmclusterLabel:        "true",
			"pmcluster.base":      "cert",
			"pmcluster.data_hash": dataHash(seed),
		},
	}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	if err := s.SetSetting(context.Background(), secretHashKey("cert"), dataHash(seed)); err != nil {
		t.Fatalf("seed hash: %v", err)
	}

	name, created, err := EnsureVersionedSecret(context.Background(), f, s, "cert", []byte("new cert bytes"))
	if err != nil {
		t.Fatalf("EnsureVersionedSecret: %v", err)
	}
	if !created {
		t.Error("created = false, want true (cert changed → new version)")
	}
	if name != "cert_v002" {
		t.Errorf("name = %q, want cert_v002", name)
	}
}
