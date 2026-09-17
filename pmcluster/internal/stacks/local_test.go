package stacks

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

func seedDeploy(t *testing.T, st *store.Store, name string, revision int64, source, rendered string) {
	t.Helper()
	if err := st.RecordDeploy(context.Background(), &store.StackRevision{
		StackName:    name,
		Revision:     revision,
		SourceYAML:   source,
		RenderedYAML: rendered,
		PayloadJSON:  sql.NullString{String: `{"app_name":"` + name + `"}`, Valid: true},
	}, "https://github.com/org/"+name); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}
}

func TestLocalGet(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedDeploy(t, st, "app", 1000, "source: yaml", "rendered: yaml")

	svc := Local{Store: st}
	got, err := svc.Get(ctx, "app")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "app" {
		t.Errorf("Name = %q, want app", got.Name)
	}
	if got.CurrentRevision != 1000 {
		t.Errorf("CurrentRevision = %d, want 1000", got.CurrentRevision)
	}
	if got.RepoURL != "https://github.com/org/app" {
		t.Errorf("RepoURL = %q", got.RepoURL)
	}

	if _, err := svc.Get(ctx, "missing"); !errors.Is(err, store.ErrStackNotFound) {
		t.Errorf("Get(missing) = %v, want ErrStackNotFound", err)
	}
}

func TestLocalList(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedDeploy(t, st, "alpha", 1, "s", "r")
	seedDeploy(t, st, "beta", 1, "s", "r")

	svc := Local{Store: st}
	rows, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("List = %d stacks, want 2", len(rows))
	}
	if rows[0].Name != "alpha" || rows[1].Name != "beta" {
		t.Errorf("List order = [%s %s], want sorted by name", rows[0].Name, rows[1].Name)
	}
}

func TestLocalRevisions(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedDeploy(t, st, "app", 100, "v1 source", "v1 rendered")
	seedDeploy(t, st, "app", 200, "v2 source", "v2 rendered")

	svc := Local{Store: st}
	revs, err := svc.Revisions(ctx, "app", 0)
	if err != nil {
		t.Fatalf("Revisions: %v", err)
	}
	if len(revs) != 2 {
		t.Fatalf("Revisions = %d, want 2", len(revs))
	}
	if revs[0].Revision != 200 {
		t.Errorf("newest revision = %d, want 200 (descending order)", revs[0].Revision)
	}
	if revs[0].SourceYAML != "v2 source" || revs[0].RenderedYAML != "v2 rendered" {
		t.Errorf("revision 200 = %+v", revs[0])
	}
	if revs[0].PayloadJSON != `{"app_name":"app"}` {
		t.Errorf("PayloadJSON = %q", revs[0].PayloadJSON)
	}

	limited, err := svc.Revisions(ctx, "app", 1)
	if err != nil {
		t.Fatalf("Revisions(limit=1): %v", err)
	}
	if len(limited) != 1 || limited[0].Revision != 200 {
		t.Errorf("Revisions(limit=1) = %+v, want just revision 200", limited)
	}
}
