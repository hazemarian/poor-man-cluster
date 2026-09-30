package usage

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
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

// TestLocalUsageFindsDslSourceRefs is the regression test for the reported
// bug: app stacks whose config() refs are resolved into env CONTENT at
// translation time have NO top-level configs: block in the rendered compose,
// so the usage graph must also scan the source DSL manifest.
func TestLocalUsageFindsDslSourceRefs(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if err := st.RecordDeploy(ctx, &store.StackRevision{
		StackName:    "donation-campaign",
		Revision:     1,
		SourceYAML:   "app: donation-campaign\nservices:\n  api:\n    env:\n      JWT_SECRET: config(donation_campaign_jwt_secret)\n      R2_KEY: config(donation_campaign_r2_access_key_id)\n",
		RenderedYAML: "services:\n  api:\n    environment:\n      JWT_SECRET: real-jwt-value\n      R2_KEY: real-key\nsecrets:\n  donation_campaign_db_password:\n    external: true\n",
		RenderedHash: "h1",
	}, ""); err != nil {
		t.Fatal(err)
	}

	l := NewLocal(st)
	got, err := l.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Configs["donation_campaign_jwt_secret"]) != 1 || got.Configs["donation_campaign_jwt_secret"][0] != "donation-campaign" {
		t.Errorf("configs[donation_campaign_jwt_secret] = %v, want [donation-campaign]", got.Configs["donation_campaign_jwt_secret"])
	}
	if len(got.Configs["donation_campaign_r2_access_key_id"]) != 1 {
		t.Errorf("configs[r2_access_key_id] = %v, want [donation-campaign]", got.Configs["donation_campaign_r2_access_key_id"])
	}
	if len(got.Secrets["donation_campaign_db_password"]) != 1 {
		t.Errorf("secrets[donation_campaign_db_password] = %v, want [donation-campaign]", got.Secrets["donation_campaign_db_password"])
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
