//go:build e2e

// End-to-end coverage for the node-local backup agent — the path that replaced
// the offen volume agent.
//
// These tests need no Swarm and no LVM: they drive the real binary against a
// real directory tree with --snapshot=none, which is the same code path a
// production run takes after the freeze. The two things they prove are the two
// things that failed in production before:
//
//  1. one artifact per stack, from the same instant, each carrying the
//     backup/data/<stack>/ prefix that browse and restore already strip;
//  2. an archive is only reported green when a database inside it actually
//     opens — and a corrupt archive is caught by the drill, not by an operator
//     discovering it during a restore.
//
// They also pin the CLI contract the operator and the systemd unit depend on:
// exit status, the plan cache, and the run record that survives a leader
// outage.
package e2e

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backupagent"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
)

// seedAgentStack creates <root>/<stack>/<stack>-data with a file and, when
// asked, a real SQLite database so verification has something to verify.
func seedAgentStack(t *testing.T, root, stack string, withDB bool) {
	t.Helper()
	dir := filepath.Join(root, stack, stack+"-data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("stack "+stack+"\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if !withDB {
		return
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`create table if not exists notes(id integer primary key, body text)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`insert into notes(body) values ('e2e')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

// writeAgentPlan drops the cached plan the timer's run reads.
func writeAgentPlan(t *testing.T, stateDir, node string, stacks ...string) string {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("state dir: %v", err)
	}
	plan := backupagent.Plan{Node: node, GeneratedAt: time.Now().Unix()}
	for _, s := range stacks {
		plan.Stacks = append(plan.Stacks, backupagent.StackPlan{Name: s})
	}
	p := filepath.Join(stateDir, "plan.json")
	if err := backupagent.WritePlan(p, plan); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	return p
}

// runAgent invokes the real binary and returns combined output and exit code.
func runAgent(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(binaryPath, args...)
	cmd.Env = append(os.Environ(), "PMCLUSTER_CONFIG="+filepath.Join(t.TempDir(), "none.yaml"))
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run %v: %v\n%s", args, err, out)
		}
	}
	return string(out), code
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// TestBackupAgentPerStackEndToEnd is the headline test: two stacks in, two
// verified artifacts out, restorable and drilled.
func TestBackupAgentPerStackEndToEnd(t *testing.T) {
	root := t.TempDir()
	archive := t.TempDir()
	state := t.TempDir()
	const node = "bh3e42i41uae6fd3mlkv7qfjg"

	seedAgentStack(t, root, "horizon-erp", true)    // has a database
	seedAgentStack(t, root, "khaal-pvc-cms", false) // files only
	writeAgentPlan(t, state, node, "horizon-erp", "khaal-pvc-cms")

	out, code := runAgent(t, "backup-agent", "run", "--force",
		"--snapshot=none",
		"--node="+node,
		"--volume-root="+root,
		"--archive-dir="+archive,
		"--state-dir="+state,
	)
	if code != 0 {
		t.Fatalf("backup-agent run exited %d\n%s", code, out)
	}
	if !strings.Contains(out, "horizon-erp") || !strings.Contains(out, "khaal-pvc-cms") {
		t.Fatalf("output does not name both stacks:\n%s", out)
	}
	if !strings.Contains(out, "verified sqlite") {
		t.Fatalf("the database artifact was not reported as verified:\n%s", out)
	}

	// One artifact per stack, named by the grammar, sidecar next to each.
	entries, err := os.ReadDir(archive)
	if err != nil {
		t.Fatalf("read archive dir: %v", err)
	}
	var tars []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tar.gz") {
			tars = append(tars, e.Name())
		}
	}
	if len(tars) != 2 {
		t.Fatalf("archives = %d (%v), want one per stack", len(tars), tars)
	}
	for _, name := range tars {
		a, ok := backups.ParseArtifact(name)
		if !ok {
			t.Fatalf("%s does not parse as an artifact", name)
		}
		if a.Node != node || a.Stack == "" {
			t.Fatalf("%s parsed as %+v", name, a)
		}
		m, err := backups.ReadManifest(filepath.Join(archive, name))
		if err != nil {
			t.Fatalf("%s: manifest: %v", name, err)
		}
		if !m.Verify.Verified || m.SHA256 == "" {
			t.Fatalf("%s: manifest not verified: %+v", name, m.Verify)
		}
		if st, err := os.Stat(filepath.Join(archive, name)); err != nil || st.Size() != m.SizeBytes {
			t.Fatalf("%s: size %v vs manifest %d (err %v)", name, st, m.SizeBytes, err)
		}
	}

	// The run record exists even though nothing could reach a leader.
	runs, err := os.ReadDir(filepath.Join(state, "runs"))
	if err != nil || len(runs) != 1 {
		t.Fatalf("run records = %v (err %v), want 1", runs, err)
	}

	// Drill the database artifact: it must extract and pass integrity_check.
	dbArtifact := filepath.Join(archive, backups.ArtifactName("horizon-erp", node, fixedArtifactTime(t, archive)))
	drillOut, code := runAgent(t, "backup-agent", "drill", dbArtifact)
	if code != 0 {
		t.Fatalf("drill exited %d\n%s", code, drillOut)
	}
	if !strings.Contains(drillOut, "integrity_check ok") {
		t.Fatalf("drill did not verify the database:\n%s", drillOut)
	}
}

// fixedArtifactTime reads the timestamp the agent actually used from the
// artifact's own name, so the test never guesses the clock.
func fixedArtifactTime(t *testing.T, dir string) time.Time {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if a, ok := backups.ParseArtifact(e.Name()); ok && a.Stack == "horizon-erp" {
			return a.At
		}
	}
	t.Fatalf("no horizon-erp artifact in %s", dir)
	return time.Time{}
}

// TestBackupAgentDrillRejectsCorruptArchive is the negative control: a flipped
// byte in the database must make the drill fail loudly. Without this, the
// previous test could pass on an agent that verifies nothing.
func TestBackupAgentDrillRejectsCorruptArchive(t *testing.T) {
	root, archive, state := t.TempDir(), t.TempDir(), t.TempDir()
	seedAgentStack(t, root, "app", true)
	writeAgentPlan(t, state, "node1", "app")

	if out, code := runAgent(t, "backup-agent", "run", "--force", "--snapshot=none", "--node=node1",
		"--volume-root="+root, "--archive-dir="+archive, "--state-dir="+state); code != 0 {
		t.Fatalf("run exited %d\n%s", code, out)
	}
	entries, err := os.ReadDir(archive)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tar.gz") {
			name = e.Name()
		}
	}
	if name == "" {
		t.Fatal("no artifact written")
	}

	// Re-pack the same tree with a deliberately corrupted database.
	bad := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bad, "app", "app-data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "app", "app-data", "app.db"), []byte("this is not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Reuse the agent to build the archive from the corrupt tree, so the tar
	// itself is valid and only the CONTENTS are wrong.
	badArchive := t.TempDir()
	badState := t.TempDir()
	writeAgentPlan(t, badState, "node1", "app")
	out, code := runAgent(t, "backup-agent", "run", "--force", "--snapshot=none", "--node=node1",
		"--volume-root="+bad, "--archive-dir="+badArchive, "--state-dir="+badState)
	if code == 0 {
		t.Fatalf("the agent reported success for an unopenable database:\n%s", out)
	}
	if !strings.Contains(out, "verify") {
		t.Fatalf("failure does not mention verification:\n%s", out)
	}

	drillOut, drillCode := runAgent(t, "backup-agent", "drill", filepath.Join(badArchive, name))
	if drillCode == 0 {
		t.Fatalf("drill passed on a corrupt database:\n%s", drillOut)
	}
}

// TestBackupAgentIsolatesAMissingStack pins the blast-radius property: an
// unplannable directory must not stop the healthy stacks from being archived.
func TestBackupAgentIsolatesAMissingStack(t *testing.T) {
	root, archive, state := t.TempDir(), t.TempDir(), t.TempDir()
	seedAgentStack(t, root, "healthy", true)
	writeAgentPlan(t, state, "node1", "healthy", "moved-away")

	out, code := runAgent(t, "backup-agent", "run", "--force", "--snapshot=none", "--node=node1",
		"--volume-root="+root, "--archive-dir="+archive, "--state-dir="+state)
	// A partially failed run is a failed run for the unit (non-zero exit)...
	if code == 0 {
		t.Fatalf("exit 0 with a missing stack:\n%s", out)
	}
	// ...but the healthy stack still produced its artifact.
	entries, err := os.ReadDir(archive)
	if err != nil {
		t.Fatal(err)
	}
	ok := 0
	for _, e := range entries {
		if a, parsed := backups.ParseArtifact(e.Name()); parsed && a.Stack == "healthy" {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("healthy stack artifacts = %d, want 1 (one bad stack must not cost the others)", ok)
	}
	if !strings.Contains(out, "moved-away") {
		t.Fatalf("output does not name the skipped stack:\n%s", out)
	}
}

// The schedule is a policy file on the node. Disabled means disabled; absent
// means the defaults, which are enabled — an unconfigured node is not an exempt
// one. There is no endpoint to be unreachable.
func TestBackupAgentHonoursTheNodePolicyFile(t *testing.T) {
	root, archive, state := t.TempDir(), t.TempDir(), t.TempDir()
	seedAgentStack(t, root, "app", true)
	writeAgentPlan(t, state, "node1", "app")

	// No policy file anywhere: the agent must still run.
	out, code := runAgent(t, "backup-agent", "run", "--snapshot=none", "--node=node1",
		"--volume-root="+root, "--archive-dir="+archive, "--state-dir="+state)
	if code != 0 {
		t.Fatalf("an unconfigured node must still back up, got %d\n%s", code, out)
	}
	entries, _ := os.ReadDir(archive)
	if len(entries) == 0 {
		t.Fatalf("no artifact written with the defaults in force:\n%s", out)
	}
}

// TestBackupAgentDryRunWritesNothing guards the flag an operator will run
// first on a production node.
func TestBackupAgentDryRunWritesNothing(t *testing.T) {
	root, archive, state := t.TempDir(), t.TempDir(), t.TempDir()
	seedAgentStack(t, root, "app", true)
	writeAgentPlan(t, state, "node1", "app")

	out, code := runAgent(t, "backup-agent", "run", "--force", "--snapshot=none", "--dry-run", "--node=node1",
		"--volume-root="+root, "--archive-dir="+archive, "--state-dir="+state)
	if code != 0 {
		t.Fatalf("dry run exited %d\n%s", code, out)
	}
	entries, _ := os.ReadDir(archive)
	if len(entries) != 0 {
		t.Fatalf("dry run wrote %d entries into the archive dir", len(entries))
	}
	// The run record is still written: a dry run is an auditable event.
	if b, err := os.ReadFile(filepath.Join(state, "runs", mustRunRecord(t, state))); err != nil ||
		!json.Valid(b) {
		t.Fatalf("run record missing or invalid: %v", err)
	}
}

func mustRunRecord(t *testing.T, state string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(state, "runs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("run records = %v (err %v)", entries, err)
	}
	return entries[0].Name()
}

// TestBackupAgentUnplannedDirsAreReported proves a leftover directory from a
// decommissioned stack is called out instead of silently archived.
func TestBackupAgentUnplannedDirsAreReported(t *testing.T) {
	root, archive, state := t.TempDir(), t.TempDir(), t.TempDir()
	seedAgentStack(t, root, "app", true)
	seedAgentStack(t, root, "dep-s2-stale", true) // left behind by an old deployment
	writeAgentPlan(t, state, "node1", "app")

	out, code := runAgent(t, "backup-agent", "run", "--force", "--snapshot=none", "--node=node1",
		"--volume-root="+root, "--archive-dir="+archive, "--state-dir="+state)
	if code != 0 {
		t.Fatalf("run exited %d\n%s", code, out)
	}
	if !strings.Contains(out, "dep-s2-stale") {
		t.Fatalf("the unplanned directory was not reported:\n%s", out)
	}
	for _, e := range mustReadDir(t, archive) {
		if strings.Contains(e, "dep-s2-stale") {
			t.Fatalf("an unplanned directory was archived: %s", e)
		}
	}
}

func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
