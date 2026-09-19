package usage

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// TestLocalUsageParsesRenderedCompose seeds two stacks whose latest revisions
// reference configs/secrets and asserts the graph is correct and deduped.
func TestLocalUsageParsesRenderedCompose(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	seed := []struct {
		stack    string
		rendered string
	}{
		{"web", "configs:\n  shared:\n    external: true\nsecrets:\n  db_pass:\n    external: true\n"},
		{"api", "configs:\n  shared:\n    external: true\nsecrets:\n  api_key:\n    external: true\n"},
	}
	for _, s := range seed {
		if err := st.RecordDeploy(ctx, &store.StackRevision{
			StackName:    s.stack,
			Revision:     1,
			SourceYAML:   "app: " + s.stack,
			RenderedYAML: s.rendered,
			RenderedHash: "h" + s.stack,
		}, ""); err != nil {
			t.Fatal(err)
		}
	}

	l := NewLocal(st)
	got, err := l.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// "shared" config referenced by both stacks; "db_pass" only by web.
	if len(got.Configs["shared"]) != 2 {
		t.Errorf("configs[shared] = %v, want [api web]", got.Configs["shared"])
	}
	if len(got.Secrets["db_pass"]) != 1 || got.Secrets["db_pass"][0] != "web" {
		t.Errorf("secrets[db_pass] = %v, want [web]", got.Secrets["db_pass"])
	}
	if len(got.Secrets["api_key"]) != 1 || got.Secrets["api_key"][0] != "api" {
		t.Errorf("secrets[api_key] = %v, want [api]", got.Secrets["api_key"])
	}
}

// TestParseComposeReferences covers malformed and empty renders.
func TestParseComposeReferences(t *testing.T) {
	cfg, sec := parseComposeReferences([]byte("not: [valid yaml"))
	if cfg != nil || sec != nil {
		t.Errorf("invalid yaml: cfg=%v sec=%v, want nil/nil", cfg, sec)
	}
	cfg, sec = parseComposeReferences([]byte("version: \"3.9\""))
	if len(cfg) != 0 || len(sec) != 0 {
		t.Errorf("empty refs: cfg=%v sec=%v, want empty", cfg, sec)
	}
}
