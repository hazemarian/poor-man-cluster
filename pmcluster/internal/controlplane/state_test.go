package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// fakeClient is an in-memory swarm config store: enough of runtime.Client for the key
// family (create, list by label, inspect, remove).
type fakeClient struct {
	runtime.Client
	configs map[string]runtime.ConfigInspectResult
}

func newFake() *fakeClient {
	return &fakeClient{configs: map[string]runtime.ConfigInspectResult{}}
}

func (f *fakeClient) ConfigCreate(_ context.Context, spec runtime.ConfigSpec) error {
	data := make([]byte, len(spec.Data))
	copy(data, spec.Data)
	f.configs[spec.Name] = runtime.ConfigInspectResult{Labels: spec.Labels, Data: data}
	return nil
}

func (f *fakeClient) ConfigList(_ context.Context, labelKey, labelValue string) ([]string, error) {
	var out []string
	for name, c := range f.configs {
		if c.Labels[labelKey] == labelValue {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeClient) ConfigInspect(_ context.Context, name string) (runtime.ConfigInspectResult, error) {
	return f.configs[name], nil
}

func (f *fakeClient) ConfigRemove(_ context.Context, name string) error {
	delete(f.configs, name)
	return nil
}

func newKeyKit(t *testing.T, dc runtime.Client, key []byte) *Kit {
	t.Helper()
	dir := t.TempDir()
	if key != nil {
		if err := os.WriteFile(filepath.Join(dir, ".encryption_key"), key, 0o600); err != nil {
			t.Fatalf("write key: %v", err)
		}
	}
	return &Kit{Docker: dc, DataDir: dir, Log: zerolog.Nop()}
}

// TestPublishKey_PublishesOnceAndOnlyOnAChange: the key config is republished when the
// bytes move (a rotation) and at no other time. Comparing CONTENT is what stops the old
// failure — a kit that looked newer on every tick because it compared mtimes.
func TestPublishKey_PublishesOnceAndOnlyOnAChange(t *testing.T) {
	ctx := context.Background()
	dc := newFake()
	kit := newKeyKit(t, dc, []byte(clusterKey))

	if err := kit.PublishKey(ctx); err != nil {
		t.Fatalf("PublishKey: %v", err)
	}
	names, _ := dc.ConfigList(ctx, LabelKind, KindKey)
	if len(names) != 1 {
		t.Fatalf("after the first publish: %d key config(s), want 1 (%v)", len(names), names)
	}
	first := names[0]

	// Nothing changed: a second pass must publish nothing.
	if err := kit.PublishKey(ctx); err != nil {
		t.Fatalf("PublishKey (unchanged): %v", err)
	}
	names, _ = dc.ConfigList(ctx, LabelKind, KindKey)
	if len(names) != 1 || names[0] != first {
		t.Fatalf("an unchanged key was republished: %v", names)
	}

	// A rotation publishes a new one, and the old one is pruned once there are more than
	// KeepKeyConfigs.
	if err := os.WriteFile(filepath.Join(kit.DataDir, ".encryption_key"), []byte("ffffffffffffffffffffffffffffffff"), 0o600); err != nil {
		t.Fatalf("rotate key: %v", err)
	}
	if err := kit.PublishKey(ctx); err != nil {
		t.Fatalf("PublishKey (rotated): %v", err)
	}
	names, _ = dc.ConfigList(ctx, LabelKind, KindKey)
	if len(names) != 2 {
		t.Fatalf("after a rotation: %d key config(s), want 2 (%v)", len(names), names)
	}

	_, key, err := kit.newestKey(ctx)
	if err != nil {
		t.Fatalf("newestKey: %v", err)
	}
	if string(key) != "ffffffffffffffffffffffffffffffff" {
		t.Errorf("newest key config carries %q, want the rotated key", key)
	}
}

func TestPublishKey_NoKeyFilePublishesNothing(t *testing.T) {
	ctx := context.Background()
	dc := newFake()
	kit := newKeyKit(t, dc, nil)
	if err := kit.PublishKey(ctx); err != nil {
		t.Fatalf("PublishKey: %v", err)
	}
	if names, _ := dc.ConfigList(ctx, LabelKind, KindKey); len(names) != 0 {
		t.Errorf("a node with no key published %v", names)
	}
}

func TestNewestKey_NoConfigIsErrNoKeyConfig(t *testing.T) {
	kit := newKeyKit(t, newFake(), []byte(clusterKey))
	if _, _, err := kit.newestKey(context.Background()); err != ErrNoKeyConfig {
		t.Fatalf("newestKey on an empty swarm = %v, want ErrNoKeyConfig", err)
	}
}

// TestPrune_KeepsTheNewestKeyConfigs: a rotation loop must not grow the swarm's config
// list without bound.
func TestPrune_KeepsTheNewestKeyConfigs(t *testing.T) {
	ctx := context.Background()
	dc := newFake()
	for i := 1; i <= 5; i++ {
		name := KeyNamePrefix + "_" + string(rune('0'+i))
		if err := dc.ConfigCreate(ctx, runtime.ConfigSpec{
			Name:   name,
			Data:   []byte(clusterKey),
			Labels: map[string]string{LabelKind: KindKey, LabelSnapshotAt: time.Now().Format(time.RFC3339Nano)},
		}); err != nil {
			t.Fatalf("seed config %d: %v", i, err)
		}
	}
	kit := &Kit{Docker: dc, Log: zerolog.Nop()}
	if err := kit.prune(ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}
	names, _ := dc.ConfigList(ctx, LabelKind, KindKey)
	if len(names) != KeepKeyConfigs {
		t.Errorf("after prune: %d key config(s), want %d (%v)", len(names), KeepKeyConfigs, names)
	}
	if names[len(names)-1] != KeyNamePrefix+"_5" {
		t.Errorf("prune kept the wrong configs: %v", names)
	}
}
