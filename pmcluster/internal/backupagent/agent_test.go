package backupagent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
)

// fakeSnap is a Snapshotter whose behaviour each test steers. It records the
// call sequence so the freeze/thaw ordering — the one thing that must never be
// wrong on a live node — is asserted rather than assumed.
type fakeSnap struct {
	kind      string
	freezeErr error
	snapErr   error
	usage     float64
	calls     []string
	dir       string
	token     string
}

func (f *fakeSnap) Kind() string { return f.kind }
func (f *fakeSnap) Freeze(context.Context, string) error {
	f.calls = append(f.calls, "freeze")
	return f.freezeErr
}
func (f *fakeSnap) Thaw(context.Context, string) error {
	f.calls = append(f.calls, "thaw")
	return nil
}
func (f *fakeSnap) Snapshot(_ context.Context, root string, _ time.Time) (string, string, error) {
	f.calls = append(f.calls, "snapshot")
	if f.snapErr != nil {
		return "", "", f.snapErr
	}
	if f.dir != "" {
		return f.dir, f.token, nil
	}
	return root, f.token, nil
}
func (f *fakeSnap) Usage(context.Context) (float64, error) {
	f.calls = append(f.calls, "usage")
	return f.usage, nil
}
func (f *fakeSnap) Drop(_ context.Context, token string) error {
	f.calls = append(f.calls, "drop:"+token)
	return nil
}

func fixedNow() time.Time { return time.Date(2026, 11, 2, 19, 0, 0, 0, time.UTC) }

// seedStack creates <root>/<stack>/<vol>/ with a file and, when withDB, a real
// SQLite database so verification has something to verify.
func seedStack(t *testing.T, root, stack string, withDB bool) {
	t.Helper()
	dir := filepath.Join(root, stack, stack+"-data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !withDB {
		return
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("create table t(id integer primary key, note text)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("insert into t(note) values ('before-snapshot')"); err != nil {
		t.Fatal(err)
	}
}

// tarMembers lists the member names of an artifact.
func tarMembers(t *testing.T, p string) []string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var out []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("tar %s: %v", p, err)
		}
		out = append(out, h.Name)
	}
}

func newAgent(t *testing.T, root, archive string, snap Snapshotter) *Agent {
	t.Helper()
	return &Agent{
		Node:       "bh3e42i41uae6fd3mlkv7qfjg",
		VolumeRoot: root,
		ArchiveDir: archive,
		Snapshot:   snap,
		Now:        fixedNow,
	}
}

func TestRunWritesOneVerifiedArtifactPerStack(t *testing.T) {
	root, archive := t.TempDir(), t.TempDir()
	seedStack(t, root, "horizon-erp", true)
	seedStack(t, root, "khaal-pvc-cms", true)
	a := newAgent(t, root, archive, &fakeSnap{kind: "lvm", usage: 4.5})

	res, err := a.Run(context.Background(), Plan{
		Node:       a.Node,
		VolumeRoot: root,
		Stacks:     []StackPlan{{Name: "horizon-erp"}, {Name: "khaal-pvc-cms"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Succeeded() {
		t.Fatalf("run not successful: %+v", res.Stacks)
	}
	if len(res.Stacks) != 2 {
		t.Fatalf("stacks = %d, want 2", len(res.Stacks))
	}

	wantName := backups.ArtifactName("horizon-erp", a.Node, fixedNow())
	if got := res.Stacks[0].Archive; got != wantName {
		t.Fatalf("archive = %q, want %q", got, wantName)
	}
	// The member prefix is the compatibility contract: browse and restore
	// strip exactly this, and nothing else changed for them.
	members := tarMembers(t, filepath.Join(archive, wantName))
	foundDB := false
	for _, m := range members {
		if !strings.HasPrefix(m, "backup/data/horizon-erp/") {
			t.Errorf("member %q does not carry the backup/data/<stack>/ prefix", m)
		}
		// The stack name must appear exactly ONCE. Repeating it pushes every
		// restored file one directory too deep, and that is invisible to a mere
		// prefix check — which is how the first version shipped this.
		if strings.HasPrefix(m, "backup/data/horizon-erp/horizon-erp/") {
			t.Errorf("stack name duplicated in member %q", m)
		}
		// The path a restore will write is volume_root/<stack>/<volume>/<file>:
		// assert the volume level exists exactly once.
		if strings.HasSuffix(m, "app.db") && !strings.HasSuffix(m, "horizon-erp-data/app.db") {
			t.Errorf("member %q does not sit under the stack's volume directory", m)
		}
		if strings.HasSuffix(m, "app.db") {
			foundDB = true
		}
	}
	if !foundDB {
		t.Errorf("app.db missing from the archive: %v", members)
	}

	// Every stack got a verified artifact with a matching checksum.
	for _, s := range res.Stacks {
		if !s.Verified || s.Engine != "sqlite" {
			t.Errorf("%s: verified=%v engine=%q detail=%q", s.Stack, s.Verified, s.Engine, s.Detail)
		}
		m, err := backups.ReadManifest(filepath.Join(archive, s.Archive))
		if err != nil {
			t.Fatalf("%s: manifest: %v", s.Stack, err)
		}
		if m.SHA256 != s.SHA256 || m.Stack != s.Stack || m.Node != a.Node {
			t.Errorf("%s: manifest mismatch: %+v", s.Stack, m)
		}
		if m.Snapshot.Kind != "lvm" {
			t.Errorf("%s: manifest snapshot kind = %q", s.Stack, m.Snapshot.Kind)
		}
	}
}

func TestRunFreezeOrderAndDrop(t *testing.T) {
	root, archive := t.TempDir(), t.TempDir()
	seedStack(t, root, "app", true)
	snap := &fakeSnap{kind: "lvm", token: "snap-token", usage: 4.5}
	a := newAgent(t, root, archive, snap)
	if _, err := a.Run(context.Background(), Plan{Node: a.Node, Stacks: []StackPlan{{Name: "app"}}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"freeze", "snapshot", "thaw", "usage", "drop:snap-token"}
	if strings.Join(snap.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("call order = %v, want %v", snap.calls, want)
	}
}

func TestRunFreezeFailureWritesNothing(t *testing.T) {
	root, archive := t.TempDir(), t.TempDir()
	seedStack(t, root, "app", true)
	snap := &fakeSnap{kind: "lvm", freezeErr: os.ErrPermission}
	a := newAgent(t, root, archive, snap)

	res, err := a.Run(context.Background(), Plan{Node: a.Node, Stacks: []StackPlan{{Name: "app"}}})
	if err == nil {
		t.Fatal("Run succeeded, want freeze failure")
	}
	if res.Error == "" || !strings.Contains(res.Error, "freeze") {
		t.Errorf("res.Error = %q", res.Error)
	}
	// The whole point: no archive is written from a live tree.
	entries, _ := os.ReadDir(archive)
	if len(entries) != 0 {
		t.Fatalf("archive dir holds %d entries after a freeze failure, want 0", len(entries))
	}
	for _, c := range snap.calls {
		if c == "snapshot" {
			t.Fatal("snapshot taken after freeze failed")
		}
	}
}

func TestRunThawsWhenSnapshotFails(t *testing.T) {
	root, archive := t.TempDir(), t.TempDir()
	seedStack(t, root, "app", true)
	snap := &fakeSnap{kind: "lvm", snapErr: io.ErrUnexpectedEOF}
	a := newAgent(t, root, archive, snap)
	if _, err := a.Run(context.Background(), Plan{Node: a.Node, Stacks: []StackPlan{{Name: "app"}}}); err == nil {
		t.Fatal("Want a snapshot error")
	}
	if strings.Join(snap.calls, ",") != "freeze,snapshot,thaw" {
		t.Fatalf("calls = %v, want freeze,snapshot,thaw (a stalled node is worse than a missed backup)", snap.calls)
	}
}

func TestRunIsolatesOneFailingStack(t *testing.T) {
	root, archive := t.TempDir(), t.TempDir()
	seedStack(t, root, "good", true)
	// "gone" is planned but has no directory on this node: it moved away.
	a := newAgent(t, root, archive, &fakeSnap{kind: "lvm", usage: 1})
	res, err := a.Run(context.Background(), Plan{Node: a.Node, Stacks: []StackPlan{
		{Name: "good"}, {Name: "gone"},
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Succeeded() {
		t.Fatal("Succeeded() = true with a failed stack")
	}
	var good, gone Stack
	for _, s := range res.Stacks {
		switch s.Stack {
		case "good":
			good = s
		case "gone":
			gone = s
		}
	}
	if !good.OK || good.Archive == "" {
		t.Errorf("good stack did not produce an artifact: %+v", good)
	}
	if gone.OK || gone.Reason == "" {
		t.Errorf("gone stack should fail with a reason: %+v", gone)
	}
	// The healthy stack's artifact is still on disk and complete.
	if _, err := os.Stat(filepath.Join(archive, good.Archive)); err != nil {
		t.Errorf("good artifact missing: %v", err)
	}
}

func TestRunAbortsLoudlyWhenPoolIsNearlyFull(t *testing.T) {
	root, archive := t.TempDir(), t.TempDir()
	seedStack(t, root, "app", true)
	a := newAgent(t, root, archive, &fakeSnap{kind: "lvm", usage: 91})
	res, err := a.Run(context.Background(), Plan{Node: a.Node, Stacks: []StackPlan{{Name: "app"}}})
	if err == nil {
		t.Fatal("Run succeeded with the pool at 91%")
	}
	if !strings.Contains(res.Error, "thin pool") {
		t.Errorf("res.Error = %q", res.Error)
	}
	if res.Snapshot.PoolPercent != 91 {
		t.Errorf("PoolPercent = %v", res.Snapshot.PoolPercent)
	}
}

func TestVerifyRejectsACorruptDatabase(t *testing.T) {
	root, archive := t.TempDir(), t.TempDir()
	dir := filepath.Join(root, "app", "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A "database" that is not one: verification must refuse it, not pass it.
	if err := os.WriteFile(filepath.Join(dir, "app.db"), []byte("not a sqlite file at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newAgent(t, root, archive, &fakeSnap{kind: "lvm", usage: 1})
	res, err := a.Run(context.Background(), Plan{Node: a.Node, Stacks: []StackPlan{{Name: "app"}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	s := res.Stacks[0]
	if s.Verified {
		t.Fatal("a non-database file was reported as verified")
	}
	if !strings.Contains(s.Reason, "verify") {
		t.Errorf("reason does not explain the failed verification: %q", s.Reason)
	}
	if res.Succeeded() {
		t.Error("Succeeded() = true with an unverified artifact")
	}
}

func TestVerifyRecognisesAPostgresDataDir(t *testing.T) {
	root, archive := t.TempDir(), t.TempDir()
	dir := filepath.Join(root, "dep-s2", "db_data", "global")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pg_control"), []byte("ctrl"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newAgent(t, root, archive, &fakeSnap{kind: "lvm", usage: 1})
	res, err := a.Run(context.Background(), Plan{Node: a.Node, Stacks: []StackPlan{{Name: "dep-s2"}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := res.Stacks[0].Engine; got != "postgres" {
		t.Fatalf("engine = %q, want postgres", got)
	}
	// Honest detail: presence is not validity.
	if !strings.Contains(res.Stacks[0].Detail, "not validated") {
		t.Errorf("detail overclaims: %q", res.Stacks[0].Detail)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	root, archive := t.TempDir(), t.TempDir()
	seedStack(t, root, "app", true)
	a := newAgent(t, root, archive, &fakeSnap{kind: "lvm"})
	a.DryRun = true
	res, err := a.Run(context.Background(), Plan{Node: a.Node, Stacks: []StackPlan{{Name: "app"}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Stacks) != 1 || !res.Stacks[0].OK {
		t.Fatalf("dry run stacks = %+v", res.Stacks)
	}
	entries, _ := os.ReadDir(archive)
	if len(entries) != 0 {
		t.Fatalf("dry run wrote %d entries", len(entries))
	}
}

func TestUnplannedDirsFindsLeftovers(t *testing.T) {
	root := t.TempDir()
	seedStack(t, root, "planned", false)
	seedStack(t, root, "dep-s2-stale", false)
	got := UnplannedDirs(root, Plan{Stacks: []StackPlan{{Name: "planned"}}})
	if len(got) != 1 || got[0] != "dep-s2-stale" {
		t.Fatalf("UnplannedDirs = %v, want [dep-s2-stale]", got)
	}
}

func TestPlanCacheRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "plan.json")
	plan := Plan{Node: "n1", VolumeRoot: "/var/stack/data", GeneratedAt: 42,
		Stacks: []StackPlan{{Name: "a"}, {Name: "b", Volumes: []string{"data"}}}}
	if err := WritePlan(p, plan); err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	got, err := LoadPlan(p)
	if err != nil {
		t.Fatalf("LoadPlan: %v", err)
	}
	if got.Node != "n1" || len(got.Stacks) != 2 || got.Stacks[1].Volumes[0] != "data" {
		t.Fatalf("round trip = %+v", got)
	}
	if names := got.StackNames(); strings.Join(names, ",") != "a,b" {
		t.Fatalf("StackNames = %v", names)
	}
}

func TestRunRequiresNodeAndRoots(t *testing.T) {
	if _, err := (&Agent{Snapshot: &NoSnapshot{}}).Run(context.Background(), Plan{}); err == nil {
		t.Fatal("want an error without a node id")
	}
	if _, err := (&Agent{Node: "n"}).Run(context.Background(), Plan{}); err == nil {
		t.Fatal("want an error without roots")
	}
	// An empty plan is not an error: a node with nothing to archive is normal.
	res, err := (&Agent{Node: "n", VolumeRoot: t.TempDir(), ArchiveDir: t.TempDir(),
		Snapshot: &NoSnapshot{}}).Run(context.Background(), Plan{Node: "n"})
	if err != nil || len(res.Stacks) != 0 {
		t.Fatalf("empty plan: res=%+v err=%v", res, err)
	}
}

func TestResultIsPersistedForLeaderlessRuns(t *testing.T) {
	dir := t.TempDir()
	p, err := WriteResult(dir, Result{Node: "bh3e42/../x", Started: 7})
	if err != nil {
		t.Fatalf("WriteResult: %v", err)
	}
	if filepath.Dir(p) != dir {
		t.Fatalf("result escaped the dir: %s", p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"node"`) {
		t.Fatalf("result body = %s", b)
	}
}

// A drill must handle BOTH member grammars: this agent writes relative
// "backup/data/<stack>/" names, while the offen agents it replaces wrote
// absolute "/backup/data/..." ones. Drilling a real offen archive with the
// first implementation failed on exactly this, which is why it is a test.
func TestDrillHandlesAbsoluteMembersAndExtractsTheRestoreShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		member func(stack string) string
	}{
		{"agent-style relative", func(s string) string { return "backup/data/" + s + "/data/app.db" }},
		{"offen-style absolute", func(s string) string { return "/backup/data/" + s + "/data/app.db" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// A real database inside the archive, so verification has to pass.
			dbPath := filepath.Join(dir, "app.db")
			db, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("create table t(x integer); insert into t values (1)"); err != nil {
				t.Fatal(err)
			}
			db.Close()

			archive := filepath.Join(t.TempDir(), "a.tar.gz")
			f, err := os.Create(archive)
			if err != nil {
				t.Fatal(err)
			}
			gz := gzip.NewWriter(f)
			tw := tar.NewWriter(gz)
			body, err := os.ReadFile(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := tw.WriteHeader(&tar.Header{
				Name: tc.member("my-app"), Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
			tw.Close()
			gz.Close()
			f.Close()

			work := t.TempDir()
			files, engine, detail, err := Drill(archive, work)
			if err != nil {
				t.Fatalf("Drill: %v", err)
			}
			if files != 1 || engine != "sqlite" {
				t.Fatalf("files=%d engine=%q detail=%q", files, engine, detail)
			}
			// The extracted tree is what a restore produces: the prefix is
			// stripped, so an operator can see the stack's own layout.
			if _, err := os.Stat(filepath.Join(work, "my-app", "data", "app.db")); err != nil {
				t.Fatalf("drill did not extract the restore shape: %v", err)
			}
		})
	}
}

func TestDrillRefusesEscapingMembers(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "evil.tar.gz")
	f, _ := os.Create(archive)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	// A member with no backup/data prefix keeps its ".." after cleaning, so it
	// is the real escape vector. (A prefixed "/backup/data/../../x" is
	// neutralised by Clean into a path inside the work dir — safe, but worth
	// knowing the two behave differently.)
	for _, name := range []string{"../../etc/passwd", "../escape"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	f.Close()
	if _, _, _, err := Drill(archive, t.TempDir()); err == nil {
		t.Fatal("drill accepted a path that escapes the work dir")
	}
}

// The prefixed form cannot escape either: Clean collapses the traversal
// against the prefix, so whatever it writes lands INSIDE the work dir. That is
// the property that matters — where exactly inside is irrelevant.
func TestDrillNeutralisesPrefixedTraversal(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sneaky.tar.gz")
	f, _ := os.Create(archive)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "backup/data/../../etc/passwd", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	f.Close()

	parent := t.TempDir()
	work := filepath.Join(parent, "work")
	if _, _, _, err := Drill(archive, work); err != nil {
		t.Fatalf("drill: %v", err)
	}
	// Everything written must be under work.
	err := filepath.Walk(parent, func(p string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() {
			return werr
		}
		if !strings.HasPrefix(p, work+string(filepath.Separator)) {
			t.Fatalf("member escaped the work dir: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(parent, "etc")); err == nil {
		t.Fatal("member escaped into the parent directory")
	}
}

// A snapshotter that can prove the volume root gets asked before anything is
// frozen: a refusal must not leave a frozen filesystem behind.
type verifyingSnap struct {
	*fakeSnap
	err   error
	asked bool
}

func (v *verifyingSnap) Verify(context.Context, string) error { v.asked = true; return v.err }

func TestRunVerifiesBeforeFreezing(t *testing.T) {
	root, archive := t.TempDir(), t.TempDir()
	seedStack(t, root, "app", true)
	inner := &fakeSnap{kind: "lvm", usage: 1}
	snap := &verifyingSnap{fakeSnap: inner, err: errFake}
	a := newAgent(t, root, archive, snap)

	res, err := a.Run(context.Background(), Plan{Node: a.Node, Stacks: []StackPlan{{Name: "app"}}})
	if err == nil {
		t.Fatal("Run succeeded on a node that failed verification")
	}
	if !snap.asked {
		t.Fatal("Verify was never called")
	}
	for _, c := range inner.calls {
		if c == "freeze" || c == "snapshot" {
			t.Fatalf("froze or snapshotted despite a failed verification: %v", inner.calls)
		}
	}
	if res.Error == "" {
		t.Error("the refusal was not recorded in the result")
	}
	if entries, _ := os.ReadDir(archive); len(entries) != 0 {
		t.Fatalf("wrote %d artifacts after a failed verification", len(entries))
	}
}

func TestUnplannedDirsIgnoresLostAndFound(t *testing.T) {
	root := t.TempDir()
	seedStack(t, root, "app", false)
	if err := os.MkdirAll(filepath.Join(root, "lost+found"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := UnplannedDirs(root, Plan{Stacks: []StackPlan{{Name: "app"}}}); len(got) != 0 {
		t.Fatalf("UnplannedDirs = %v, want none (lost+found is a filesystem artefact)", got)
	}
}
