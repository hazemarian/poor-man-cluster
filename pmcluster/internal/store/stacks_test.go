package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// makeRevision builds a StackRevision for testing. revision=0 uses
// time.Now().Unix() so callers don't need to manage IDs manually.
func makeRevision(stackName string, revision int64, source, rendered string) *StackRevision {
	if revision == 0 {
		revision = time.Now().Unix()
	}
	return &StackRevision{
		StackName:    stackName,
		Revision:     revision,
		SourceYAML:   source,
		RenderedYAML: rendered,
		PayloadJSON:  sql.NullString{String: `{"test":true}`, Valid: true},
		SourceFile:   "deploy/mystack.yaml",
	}
}

// TestRecordDeploy_HappyPath inserts a brand-new stack with its first revision
// and verifies Stack + Revision return the expected rows.
func TestRecordDeploy_HappyPath(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rev := makeRevision("mystack", 1000, "source: yaml", "rendered: yaml")
	if err := s.RecordDeploy(ctx, rev, "https://github.com/org/repo"); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}

	st, err := s.Stack(ctx, "mystack")
	if err != nil {
		t.Fatalf("Stack: %v", err)
	}
	if st.Name != "mystack" {
		t.Errorf("stack.Name = %q, want mystack", st.Name)
	}
	if st.CurrentRevision != 1000 {
		t.Errorf("stack.CurrentRevision = %d, want 1000", st.CurrentRevision)
	}
	if !st.RepoURL.Valid || st.RepoURL.String != "https://github.com/org/repo" {
		t.Errorf("stack.RepoURL = %v, want valid https://github.com/org/repo", st.RepoURL)
	}
	if st.SourceFile != "deploy/mystack.yaml" {
		t.Errorf("stack.SourceFile = %q, want deploy/mystack.yaml", st.SourceFile)
	}

	r, err := s.Revision(ctx, "mystack", 1000)
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	if r.StackName != "mystack" {
		t.Errorf("revision.StackName = %q, want mystack", r.StackName)
	}
	if r.Revision != 1000 {
		t.Errorf("revision.Revision = %d, want 1000", r.Revision)
	}
	if r.SourceYAML != "source: yaml" {
		t.Errorf("revision.SourceYAML = %q, want 'source: yaml'", r.SourceYAML)
	}
	if r.RenderedYAML != "rendered: yaml" {
		t.Errorf("revision.RenderedYAML = %q, want 'rendered: yaml'", r.RenderedYAML)
	}
	if r.SourceFile != "deploy/mystack.yaml" {
		t.Errorf("revision.SourceFile = %q, want deploy/mystack.yaml", r.SourceFile)
	}
}

// TestRecordDeploy_SecondRevision verifies that a second deploy on the same
// stack advances current_revision and updated_at, while leaving created_at
// unchanged.
func TestRecordDeploy_SecondRevision(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rev1 := makeRevision("mystack", 1000, "src-v1", "rendered-v1")
	if err := s.RecordDeploy(ctx, rev1, "https://git/repo"); err != nil {
		t.Fatalf("RecordDeploy v1: %v", err)
	}
	st1, err := s.Stack(ctx, "mystack")
	if err != nil {
		t.Fatalf("Stack after v1: %v", err)
	}

	time.Sleep(time.Second + 10*time.Millisecond)

	rev2 := makeRevision("mystack", 2000, "src-v2", "rendered-v2")
	if err := s.RecordDeploy(ctx, rev2, "https://git/repo"); err != nil {
		t.Fatalf("RecordDeploy v2: %v", err)
	}
	st2, err := s.Stack(ctx, "mystack")
	if err != nil {
		t.Fatalf("Stack after v2: %v", err)
	}

	if st2.CurrentRevision != 2000 {
		t.Errorf("current_revision = %d, want 2000", st2.CurrentRevision)
	}

	if st2.CreatedAt != st1.CreatedAt {
		t.Errorf("created_at changed: was %d, now %d", st1.CreatedAt, st2.CreatedAt)
	}

	if st2.UpdatedAt <= st1.UpdatedAt {
		t.Errorf("updated_at did not advance: was %d, still %d", st1.UpdatedAt, st2.UpdatedAt)
	}

	if _, err := s.Revision(ctx, "mystack", 1000); err != nil {
		t.Errorf("Revision(1000): %v", err)
	}
	if _, err := s.Revision(ctx, "mystack", 2000); err != nil {
		t.Errorf("Revision(2000): %v", err)
	}
}

// TestRecordDeploy_RenderedHashRoundTrip verifies the rendered_hash column is
// persisted on RecordDeploy and read back by Revision and ListRevisions.
func TestRecordDeploy_RenderedHashRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rev := makeRevision("mystack", 1000, "source: yaml", "rendered: yaml")
	rev.RenderedHash = ConfigHash("rendered: yaml")
	if err := s.RecordDeploy(ctx, rev, ""); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}

	r, err := s.Revision(ctx, "mystack", 1000)
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	if r.RenderedHash != rev.RenderedHash {
		t.Errorf("Revision.RenderedHash = %q, want %q", r.RenderedHash, rev.RenderedHash)
	}

	rev2 := makeRevision("mystack", 1001, "source: yaml", "rendered: yaml v2")
	rev2.RenderedHash = ConfigHash("rendered: yaml v2")
	if err := s.RecordDeploy(ctx, rev2, ""); err != nil {
		t.Fatalf("RecordDeploy v2: %v", err)
	}

	revs, err := s.ListRevisions(ctx, "mystack", 0)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revs) != 2 {
		t.Fatalf("len(revs) = %d, want 2", len(revs))
	}
	// Newest first: rev2 then rev.
	if revs[0].RenderedHash != rev2.RenderedHash || revs[1].RenderedHash != rev.RenderedHash {
		t.Errorf("ListRevisions hashes = [%q, %q], want [%q, %q]",
			revs[0].RenderedHash, revs[1].RenderedHash, rev2.RenderedHash, rev.RenderedHash)
	}

	// Limited list keeps the hashes too.
	one, err := s.ListRevisions(ctx, "mystack", 1)
	if err != nil {
		t.Fatalf("ListRevisions(1): %v", err)
	}
	if len(one) != 1 || one[0].RenderedHash != rev2.RenderedHash {
		t.Errorf("ListRevisions(1) hash = %v, want [%q]", one, rev2.RenderedHash)
	}
}

// TestSetRevisionRenderedHash_StampsAndErrors verifies the deploy pipeline's
// post-apply hash setter: it stamps the rendered_hash column of an existing
// revision, is read back by Revision, and returns ErrRevisionNotFound for an
// unknown (stack, revision) pair.
func TestSetRevisionRenderedHash_StampsAndErrors(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rev := makeRevision("mystack", 1000, "source: yaml", "rendered: yaml")
	// Record with an EMPTY hash (the deploy pipeline's up-front record).
	rev.RenderedHash = ""
	if err := s.RecordDeploy(ctx, rev, ""); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}

	// Before the apply succeeds, the hash is empty.
	got, err := s.Revision(ctx, "mystack", 1000)
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	if got.RenderedHash != "" {
		t.Fatalf("RenderedHash before stamp = %q, want empty", got.RenderedHash)
	}

	want := ConfigHash("rendered: yaml")
	if err := s.SetRevisionRenderedHash(ctx, "mystack", 1000, want); err != nil {
		t.Fatalf("SetRevisionRenderedHash: %v", err)
	}

	got, err = s.Revision(ctx, "mystack", 1000)
	if err != nil {
		t.Fatalf("Revision (after stamp): %v", err)
	}
	if got.RenderedHash != want {
		t.Errorf("RenderedHash after stamp = %q, want %q", got.RenderedHash, want)
	}

	// Unknown revision errors, and a mismatched stack name errors too.
	if err := s.SetRevisionRenderedHash(ctx, "mystack", 9999, want); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("SetRevisionRenderedHash(unknown revision) = %v, want ErrRevisionNotFound", err)
	}
	if err := s.SetRevisionRenderedHash(ctx, "other-stack", 1000, want); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("SetRevisionRenderedHash(wrong stack) = %v, want ErrRevisionNotFound", err)
	}
}

// TestRecordDeploy_RepoURLPreservedOnEmptyUpdate verifies that passing an
// empty repoURL on a subsequent deploy preserves the existing value via the
// COALESCE(NULLIF(?, ”), repo_url) clause.
func TestRecordDeploy_RepoURLPreservedOnEmptyUpdate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rev1 := makeRevision("mystack", 1000, "src", "rendered")
	if err := s.RecordDeploy(ctx, rev1, "https://git/original"); err != nil {
		t.Fatalf("RecordDeploy v1: %v", err)
	}

	rev2 := makeRevision("mystack", 2000, "src2", "rendered2")
	if err := s.RecordDeploy(ctx, rev2, ""); err != nil {
		t.Fatalf("RecordDeploy v2: %v", err)
	}

	st, err := s.Stack(ctx, "mystack")
	if err != nil {
		t.Fatalf("Stack: %v", err)
	}
	if !st.RepoURL.Valid || st.RepoURL.String != "https://git/original" {
		t.Errorf("repo_url = %v, want https://git/original (preserved)", st.RepoURL)
	}
}

// TestGetStack_NotFound verifies ErrStackNotFound is returned for an unknown name.
func TestGetStack_NotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, err := s.Stack(ctx, "does-not-exist")
	if !errors.Is(err, ErrStackNotFound) {
		t.Errorf("Stack(unknown) err = %v, want ErrStackNotFound", err)
	}
}

// TestGetRevision_NotFound verifies ErrRevisionNotFound for unknown (stack, revision).
func TestGetRevision_NotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, err := s.Revision(ctx, "ghost", 9999)
	if !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("Revision(ghost, 9999) err = %v, want ErrRevisionNotFound", err)
	}

	rev := makeRevision("mystack", 1000, "src", "rendered")
	if err := s.RecordDeploy(ctx, rev, ""); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}
	_, err = s.Revision(ctx, "mystack", 9999)
	if !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("Revision(mystack, 9999) err = %v, want ErrRevisionNotFound", err)
	}
}

// TestListStacks_OrderedByName inserts stacks in b, a, c order and verifies
// ListStacks returns them in alphabetical order [a, b, c].
func TestListStacks_OrderedByName(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for _, name := range []string{"bravo", "alpha", "charlie"} {
		rev := makeRevision(name, time.Now().UnixNano()/int64(time.Millisecond), "src", "rendered")
		if err := s.RecordDeploy(ctx, rev, ""); err != nil {
			t.Fatalf("RecordDeploy(%s): %v", name, err)
		}

		time.Sleep(2 * time.Millisecond)
	}

	stacks, err := s.ListStacks(ctx)
	if err != nil {
		t.Fatalf("ListStacks: %v", err)
	}
	if len(stacks) != 3 {
		t.Fatalf("len(stacks) = %d, want 3", len(stacks))
	}
	want := []string{"alpha", "bravo", "charlie"}
	for i, st := range stacks {
		if st.Name != want[i] {
			t.Errorf("stacks[%d].Name = %q, want %q", i, st.Name, want[i])
		}
	}
}

// TestListRevisions_NewestFirst inserts three revisions and verifies they are
// returned newest-first. Also verifies that limit truncates the results.
func TestListRevisions_NewestFirst(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for _, rev := range []int64{100, 200, 300} {
		r := makeRevision("mystack", rev, "src", "rendered")
		if err := s.RecordDeploy(ctx, r, ""); err != nil {
			t.Fatalf("RecordDeploy(%d): %v", rev, err)
		}
	}

	t.Run("limit=0 returns all", func(t *testing.T) {
		revs, err := s.ListRevisions(ctx, "mystack", 0)
		if err != nil {
			t.Fatalf("ListRevisions: %v", err)
		}
		if len(revs) != 3 {
			t.Fatalf("len = %d, want 3", len(revs))
		}

		if revs[0].Revision != 300 || revs[1].Revision != 200 || revs[2].Revision != 100 {
			t.Errorf("order wrong: %v", revisionsIDs(revs))
		}
	})

	t.Run("limit=2 truncates", func(t *testing.T) {
		revs, err := s.ListRevisions(ctx, "mystack", 2)
		if err != nil {
			t.Fatalf("ListRevisions: %v", err)
		}
		if len(revs) != 2 {
			t.Fatalf("len = %d, want 2", len(revs))
		}
		if revs[0].Revision != 300 || revs[1].Revision != 200 {
			t.Errorf("order wrong: %v", revisionsIDs(revs))
		}
	})

	t.Run("limit=-1 returns all", func(t *testing.T) {
		revs, err := s.ListRevisions(ctx, "mystack", -1)
		if err != nil {
			t.Fatalf("ListRevisions: %v", err)
		}
		if len(revs) != 3 {
			t.Fatalf("len = %d, want 3", len(revs))
		}
	})
}

// TestCascadeDelete_StackDeletesRevisions verifies that deleting a stacks row
// cascades to stack_revisions (FK ON DELETE CASCADE).
func TestCascadeDelete_StackDeletesRevisions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rev := makeRevision("doomed", 5000, "src", "rendered")
	if err := s.RecordDeploy(ctx, rev, ""); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}

	if _, err := s.Revision(ctx, "doomed", 5000); err != nil {
		t.Fatalf("Revision before delete: %v", err)
	}

	if _, err := s.DB().ExecContext(ctx, `DELETE FROM stacks WHERE name = 'doomed'`); err != nil {
		t.Fatalf("DELETE FROM stacks: %v", err)
	}

	_, err := s.Revision(ctx, "doomed", 5000)
	if !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("Revision after cascade delete = %v, want ErrRevisionNotFound", err)
	}

	_, err = s.Stack(ctx, "doomed")
	if !errors.Is(err, ErrStackNotFound) {
		t.Errorf("Stack after delete = %v, want ErrStackNotFound", err)
	}
}

// TestDeleteStack_RemovesStackAndRevisions deletes a deployed stack via the
// store API: the stack row AND its revisions (ON DELETE CASCADE) are gone.
func TestDeleteStack_RemovesStackAndRevisions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rev := makeRevision("mystack", 1000, "src", "rendered")
	if err := s.RecordDeploy(ctx, rev, "https://github.com/org/repo"); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}
	rev2 := makeRevision("mystack", 1001, "src2", "rendered2")
	if err := s.RecordDeploy(ctx, rev2, "https://github.com/org/repo"); err != nil {
		t.Fatalf("RecordDeploy v2: %v", err)
	}

	// The stack owns service-scope configs + secrets — deleting the stack
	// must remove them too (no orphans).
	if _, err := s.CreateConfig(ctx, "service", "mystack", "mystack_env", "env", "A=1", "v1"); err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	if _, err := s.CreateSecret(ctx, "service", "mystack", "mystack_db_pass", []byte("cipher"), "h1"); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if _, err := s.CreateConfig(ctx, "cluster", "", "traefik-dynamic", "template", "x", "v1"); err != nil {
		t.Fatalf("CreateConfig cluster: %v", err)
	}

	if err := s.DeleteStack(ctx, "mystack"); err != nil {
		t.Fatalf("DeleteStack: %v", err)
	}

	if _, err := s.Stack(ctx, "mystack"); !errors.Is(err, ErrStackNotFound) {
		t.Errorf("Stack after delete = %v, want ErrStackNotFound", err)
	}
	if _, err := s.Revision(ctx, "mystack", 1000); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("Revision(1000) after delete = %v, want ErrRevisionNotFound (cascade)", err)
	}
	if _, err := s.Revision(ctx, "mystack", 1001); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("Revision(1001) after delete = %v, want ErrRevisionNotFound (cascade)", err)
	}
	if _, err := s.Config(ctx, "mystack_env"); !errors.Is(err, ErrConfigNotFound) {
		t.Errorf("Config after delete = %v, want ErrConfigNotFound", err)
	}
	if _, err := s.Secret(ctx, "mystack_db_pass"); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("Secret after delete = %v, want ErrSecretNotFound", err)
	}
	// Cluster-scope rows are untouched.
	if _, err := s.Config(ctx, "traefik-dynamic"); err != nil {
		t.Errorf("Config cluster row after delete: %v, want nil", err)
	}
}

// TestDeleteStack_UnknownStack verifies DeleteStack reports ErrStackNotFound
// when no stack row matches.
func TestDeleteStack_UnknownStack(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.DeleteStack(ctx, "ghost"); !errors.Is(err, ErrStackNotFound) {
		t.Errorf("DeleteStack(ghost) = %v, want ErrStackNotFound", err)
	}
}

// TestSetStackLastError_RoundTrip verifies a stack's last deploy/apply error
// is persisted, read back, cleared, and that unknown stacks error out.
func TestSetStackLastError_RoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.RecordDeploy(ctx, makeRevision("mystack", 1000, "source: yaml", "rendered: yaml"), "https://git/repo"); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}

	// Default is empty.
	st, err := s.Stack(ctx, "mystack")
	if err != nil {
		t.Fatalf("Stack: %v", err)
	}
	if st.LastError != "" {
		t.Errorf("LastError initial = %q, want empty", st.LastError)
	}

	if err := s.SetStackLastError(ctx, "mystack", "docker stack deploy (depends_on level 1): boom"); err != nil {
		t.Fatalf("SetStackLastError: %v", err)
	}
	st, err = s.Stack(ctx, "mystack")
	if err != nil {
		t.Fatalf("Stack: %v", err)
	}
	if st.LastError != "docker stack deploy (depends_on level 1): boom" {
		t.Errorf("LastError = %q, want the recorded apply error", st.LastError)
	}

	// ListStacks surfaces it too.
	stacks, err := s.ListStacks(ctx)
	if err != nil {
		t.Fatalf("ListStacks: %v", err)
	}
	if len(stacks) != 1 || stacks[0].LastError == "" {
		t.Errorf("ListStacks LastError not surfaced: %+v", stacks)
	}

	// Clearing on the next successful deploy.
	if err := s.SetStackLastError(ctx, "mystack", ""); err != nil {
		t.Fatalf("SetStackLastError clear: %v", err)
	}
	st, _ = s.Stack(ctx, "mystack")
	if st.LastError != "" {
		t.Errorf("LastError after clear = %q, want empty", st.LastError)
	}

	// Unknown stack.
	if err := s.SetStackLastError(ctx, "ghost", "x"); !errors.Is(err, ErrStackNotFound) {
		t.Errorf("SetStackLastError(ghost) = %v, want ErrStackNotFound", err)
	}
}

// TestRecordStackError_JSONHistory verifies the last_error column behaves as a
// JSON outcome history: failures append newest-first, a success entry masks
// older failures for the display while the history is retained, the column is
// capped, and unknown stacks error out.
func TestRecordStackError_JSONHistory(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.RecordDeploy(ctx, makeRevision("mystack", 1000, "source: yaml", "rendered: yaml"), "https://git/repo"); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}

	// Two failures, newest first.
	if _, err := s.RecordStackError(ctx, "mystack", 1000, "boom one"); err != nil {
		t.Fatalf("RecordStackError: %v", err)
	}
	if _, err := s.RecordStackError(ctx, "mystack", 1001, "boom two"); err != nil {
		t.Fatalf("RecordStackError: %v", err)
	}
	entries, err := s.ListStackErrors(ctx, "mystack", 0)
	if err != nil {
		t.Fatalf("ListStackErrors: %v", err)
	}
	if len(entries) != 2 || entries[0].Error != "boom two" || entries[1].Error != "boom one" {
		t.Errorf("history = %+v, want newest-first [boom two, boom one]", entries)
	}
	if entries[0].Revision != 1001 || entries[1].Revision != 1000 {
		t.Errorf("history revisions = %+v, want [1001, 1000]", entries)
	}

	// A clean apply writes NOTHING — no success entries in the history.
	if _, err := s.RecordStackError(ctx, "mystack", 1002, ""); err != nil {
		t.Fatalf("RecordStackError success: %v", err)
	}
	st, err := s.Stack(ctx, "mystack")
	if err != nil {
		t.Fatalf("Stack: %v", err)
	}
	all := ParseStackErrors(st.LastError)
	if len(all) != 2 {
		t.Errorf("history after clean apply = %q, want unchanged failures-only [boom two, boom one]", st.LastError)
	}
	if all[0].Error != "boom two" || all[1].Error != "boom one" {
		t.Errorf("history after clean apply = %+v, want [boom two, boom one]", all)
	}

	// limit caps the returned slice.
	capped, err := s.ListStackErrors(ctx, "mystack", 2)
	if err != nil {
		t.Fatalf("ListStackErrors limited: %v", err)
	}
	if len(capped) != 2 {
		t.Errorf("limited history len = %d, want 2", len(capped))
	}

	// Unknown stack.
	if _, err := s.RecordStackError(ctx, "ghost", 1, "x"); !errors.Is(err, ErrStackNotFound) {
		t.Errorf("RecordStackError(ghost) = %v, want ErrStackNotFound", err)
	}
}

// revisionsIDs is a debug helper that extracts revision IDs from a slice.
func revisionsIDs(revs []*StackRevision) []int64 {
	ids := make([]int64, len(revs))
	for i, r := range revs {
		ids[i] = r.Revision
	}
	return ids
}
