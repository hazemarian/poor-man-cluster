package controlplane

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// The drill is the only thing that ever exercises the control-plane restore
// outside a real leader death, so it has to be true: a kit whose key and
// ciphertext disagree must FAIL, and a healthy kit must come back with the
// secrets decrypted — not just "extracted N files".
type kitFake struct {
	// The embedded nil client satisfies runtime.Client; only the two config
	// methods are exercised, and any other call panics loudly rather than
	// silently answering zero values.
	runtime.Client
	configs map[string]runtime.ConfigInspectResult
}

func (f *kitFake) ConfigList(_ context.Context, _, value string) ([]string, error) {
	var out []string
	for name, c := range f.configs {
		if value != "" && c.Labels[LabelKind] != value {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

func (f *kitFake) ConfigCreate(_ context.Context, spec runtime.ConfigSpec) error {
	f.configs[spec.Name] = runtime.ConfigInspectResult{Labels: spec.Labels, Data: spec.Data}
	return nil
}

func (f *kitFake) ConfigExists(_ context.Context, name string) (bool, error) {
	_, ok := f.configs[name]
	return ok, nil
}

func (f *kitFake) ConfigRemove(_ context.Context, name string) error {
	delete(f.configs, name)
	return nil
}

func (f *kitFake) ConfigInspect(_ context.Context, name string) (runtime.ConfigInspectResult, error) {
	if c, ok := f.configs[name]; ok {
		return c, nil
	}
	return runtime.ConfigInspectResult{}, fmt.Errorf("config %q not found", name)
}

// buildKit snapshots a real store (with one encrypted secret) into configs, so
// the drill is fed bytes produced by the same code that publishes them.
func buildKit(t *testing.T) (*kitFake, string, []byte) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	st, err := store.Open(dir + "/data.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SetSetting(ctx, "domain", "kit.example.com"); err != nil {
		t.Fatalf("seed setting: %v", err)
	}

	// A real cipher under the kit's own key path, so the drill's decrypt probe
	// has something honest to prove.
	cipher, err := credentials.Open(dir + "/.encryption_key")
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	payload, err := cipher.Encrypt([]byte("s3cr3t"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := st.CreateSecret(ctx, "service", "app", "app_session", payload, store.SecretHash("s3cr3t")); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	f := &kitFake{configs: map[string]runtime.ConfigInspectResult{}}
	kit := &Kit{Docker: f, DataDir: dir}
	if err := kit.Snapshot(ctx); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	key, err := os.ReadFile(dir + "/.encryption_key")
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	return f, dir, key
}

func TestVerify_ProvesAHealthyKitRestores(t *testing.T) {
	f, dir, key := buildKit(t)
	kit := &Kit{Docker: f, DataDir: dir + "/unused"}

	rep, err := kit.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.Tables == 0 {
		t.Error("no tables counted — the restored DB was not opened")
	}
	if rep.SchemaVersion == "" {
		t.Error("schema version empty")
	}
	if rep.Secrets != 1 || rep.SecretsOK != 1 {
		t.Errorf("secrets = %d, decrypted = %d; want 1/1 (the point of the drill)", rep.Secrets, rep.SecretsOK)
	}
	if rep.KeyFingerprint != fingerprint(key) {
		t.Errorf("fingerprint = %q, want %q", rep.KeyFingerprint, fingerprint(key))
	}
	if len(rep.Warnings) != 0 {
		t.Errorf("healthy kit produced warnings: %#v", rep.Warnings)
	}
	if age, err := time.ParseDuration(rep.SnapshotAge); err != nil || age > time.Hour {
		t.Errorf("snapshot age = %q, want a recent snapshot", rep.SnapshotAge)
	}
}

// A kit whose key config was pruned cannot restore — and must say so instead of
// extracting a data kit it can never decrypt.
func TestVerify_RefusesAStateConfigWithoutItsKey(t *testing.T) {
	f, dir, _ := buildKit(t)
	for name, c := range f.configs {
		if c.Labels[LabelKind] == KindKey {
			delete(f.configs, name)
		}
	}
	kit := &Kit{Docker: f, DataDir: dir}

	if _, err := kit.Verify(context.Background()); err == nil {
		t.Fatal("verified a data kit with no key config")
	}
}

// The one failure the drill exists to catch: key and ciphertext from different
// generations. The restore succeeds, the DB opens, and the decryption probe fails.
func TestVerify_ReportsAKeyThatDoesNotOpenTheCiphertext(t *testing.T) {
	f, dir, _ := buildKit(t)

	// Swap in a fresh, valid-length key that never sealed the stored row.
	other := t.TempDir() + "/.encryption_key"
	if _, err := credentials.Open(other); err != nil {
		t.Fatalf("other cipher: %v", err)
	}
	foreign, err := os.ReadFile(other)
	if err != nil {
		t.Fatalf("read other key: %v", err)
	}
	for name, c := range f.configs {
		if c.Labels[LabelKind] == KindKey {
			c.Data = foreign
			f.configs[name] = c
		}
	}

	kit := &Kit{Docker: f, DataDir: dir}
	rep, err := kit.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.SecretsOK != 0 {
		t.Errorf("decrypted %d secret(s) with a foreign key", rep.SecretsOK)
	}
	if len(rep.Warnings) == 0 {
		t.Fatal("a key that cannot open the ciphertext must be reported")
	}
}
