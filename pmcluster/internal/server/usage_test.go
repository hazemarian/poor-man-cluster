package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// TestUsageAPI exercises GET /api/usage: it computes the config/secret → stack
// reference graph from the LATEST rendered compose of each stack, dedupes and
// sorts the stack lists, and rejects unauthenticated requests.
func TestUsageAPI(t *testing.T) {
	st := openServerStore(t)
	ctx := context.Background()

	// alpha: rev 100 references c1/s1, then rev 200 references only c3/s3.
	// The latest (200) must win — c1/s1 must NOT be attributed to alpha.
	if err := st.RecordDeploy(ctx, &store.StackRevision{
		StackName:    "alpha",
		Revision:     100,
		RenderedYAML: "services:\n  web:\n    image: nginx\nconfigs:\n  c1:\n    external: true\nsecrets:\n  s1:\n    external: true\n",
	}, ""); err != nil {
		t.Fatalf("RecordDeploy alpha@100: %v", err)
	}
	if err := st.RecordDeploy(ctx, &store.StackRevision{
		StackName:    "alpha",
		Revision:     200,
		RenderedYAML: "services:\n  web:\n    image: nginx\nconfigs:\n  c3:\n    external: true\nsecrets:\n  s3:\n    external: true\n",
	}, ""); err != nil {
		t.Fatalf("RecordDeploy alpha@200: %v", err)
	}
	// beta: references c1, c2 and s2.
	if err := st.RecordDeploy(ctx, &store.StackRevision{
		StackName:    "beta",
		Revision:     1,
		RenderedYAML: "services:\n  web:\n    image: nginx\nconfigs:\n  c1:\n    external: true\n  c2:\n    external: true\nsecrets:\n  s2:\n    external: true\n",
	}, ""); err != nil {
		t.Fatalf("RecordDeploy beta: %v", err)
	}

	srv := httptest.NewServer(New(Deps{
		Lookup: &fakeLookup{users: map[string]*auth.User{"tok": {ID: 1, Name: "admin"}}},
		Store:  st,
	}))
	defer srv.Close()
	base := srv.URL + "/api/usage"

	if resp := doJSON(t, http.MethodGet, base, "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth GET = %d, want 401", resp.StatusCode)
	}

	resp := doJSON(t, http.MethodGet, base, "tok", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body: %s", resp.StatusCode, readBody(t, resp))
	}
	var body struct {
		Configs map[string][]string `json:"configs"`
		Secrets map[string][]string `json:"secrets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// c1 is referenced by beta only (alpha's c1 is on a stale revision).
	if got := body.Configs["c1"]; len(got) != 1 || got[0] != "beta" {
		t.Errorf("configs[c1] = %v, want [beta]", got)
	}
	// c2 by beta, c3 by alpha (latest revision only).
	if got := body.Configs["c2"]; len(got) != 1 || got[0] != "beta" {
		t.Errorf("configs[c2] = %v, want [beta]", got)
	}
	if got := body.Configs["c3"]; len(got) != 1 || got[0] != "alpha" {
		t.Errorf("configs[c3] = %v, want [alpha]", got)
	}
	// s1 was only on alpha's stale revision — it must be absent.
	if got, ok := body.Secrets["s1"]; ok {
		t.Errorf("secrets[s1] = %v, want absent (stale revision)", got)
	}
	if got := body.Secrets["s2"]; len(got) != 1 || got[0] != "beta" {
		t.Errorf("secrets[s2] = %v, want [beta]", got)
	}
	if got := body.Secrets["s3"]; len(got) != 1 || got[0] != "alpha" {
		t.Errorf("secrets[s3] = %v, want [alpha]", got)
	}
}
