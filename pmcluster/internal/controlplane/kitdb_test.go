package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	_ "modernc.org/sqlite"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// The tests in this file are about the kit's CONTENT: which tables cross the
// fleet, whether the authoritative rows survive a pack→unpack round trip, and
// whether the payload stays inside Docker's config ceiling when the leader's
// database is dominated by deploy history. They build a real SQLite database
// through the daemon's own store rather than a fixture string, because the
// defect being fixed lived in the difference between "a bag of bytes called
// data.db" and "the real database".

const testKey = "0123456789abcdef0123456789abcdef"

// realDataDir builds a data dir whose data.db is a real, migrated control-plane
// database, seeds it, and returns the dir plus the number of revisions seeded.
//
// revisions is the size lever: the live leader had 538 of them for 3.7 MB of
// file, so a regression test needs enough to make the difference between
// "slim" and "whole database" unambiguous.
func realDataDir(t *testing.T, revisions int) (string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedAuthoritative(t, st)
	seedExcluded(t, st)
	seedRevisions(t, st, "bulk-app", revisions)
	if err := os.WriteFile(filepath.Join(dir, ".encryption_key"), []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("listen_addr: 0.0.0.0:9090\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config", "main-cert.pem"), []byte("cert-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return dir, st
}

// ciphers are the credential material the kit exists to carry. Byte-exact
// comparisons later prove the blobs survive the export, not that they decode.
var (
	testCredCipher    = []byte{0x00, 0x11, 0xde, 0xad, 0xbe, 0xef, 0xff, 0x42}
	testRegistryCiph  = []byte{0x9a, 0x00, 0x00, 0x7f, 0x13, 0x37}
	testWebhookSecret = []byte{0x55, 0xaa, 0x00, 0x01, 0xfe, 0xed}
)

func seedAuthoritative(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	must := func(err error, what string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	_, err := st.CreateUser(ctx, "admin", "tokid-admin", "argon2-hash-admin")
	must(err, "create admin user")
	_, err = st.CreateUserWithRole(ctx, "deploy-bot", "tokid-bot", "argon2-hash-bot", "operator", "test2-lms")
	must(err, "create scoped api key")

	must(st.InsertCredential(ctx, &store.ManagedCredential{
		Name:               "traefik_dashboard",
		Kind:               "traefik",
		Username:           "admin",
		PasswordCiphertext: testCredCipher,
		SwarmSecretName:    "traefik_dashboard_v1",
	}), "insert credential")

	must(st.CreateWebhookSource(ctx, "github", "CI", testWebhookSecret), "insert webhook source")
	must(st.CreateRegistry(ctx, &store.Registry{Host: "ghcr.io", Username: "bot", PasswordCiphertext: testRegistryCiph}), "insert registry")

	for k, v := range map[string]string{
		"domain":        "nextrum-sy.com",
		"tls_mode":      "acme",
		"volume_root":   "/var/stack",
		"oo_admin_mail": "ops@nextrum-sy.com",
	} {
		must(st.SetSetting(ctx, k, v), "set setting "+k)
	}

	// A cluster-scope template with one previous version (config_versions), and
	// a service-scope config belonging to a stack.
	_, err = st.CreateConfig(ctx, "cluster", "", "traefik_dynamic", "template", "# template v1\n", "v0.2.258")
	must(err, "create cluster config")
	_, err = st.UpdateConfig(ctx, "traefik_dynamic", "# template v2\n", "v0.2.259")
	must(err, "update cluster config")
	_, err = st.CreateConfig(ctx, "service", "test2-lms", "lms_env", "env", "APP_ENV=production\n", "v0.2.258")
	must(err, "create service config")

	_, err = st.CreateSecret(ctx, "cluster", "", "oo_admin_password", []byte{0xde, 0xad, 0x00, 0xbe, 0xef}, "sha256-of-plaintext")
	must(err, "create secret")

	must(st.PutSiteCert(ctx, store.SiteCertRow{
		Domain:     "nextrum-sy.com",
		CertSecret: "cert_v3",
		KeySecret:  "key_v3",
		NotBefore:  time.Unix(1_790_000_000, 0),
		NotAfter:   time.Unix(1_820_000_000, 0),
		SANs:       []string{"nextrum-sy.com", "*.nextrum-sy.com"},
		CertHash:   "cert-hash",
		KeyHash:    "key-hash",
		CreatedAt:  time.Unix(1_790_000_000, 0),
		UpdatedAt:  time.Unix(1_790_000_000, 0),
	}), "put site cert")

	must(st.SetStackFailover(ctx, store.StackFailover{
		StackName: "test2-lms", FromNode: "nxt-sw-4-m", ToNode: "nxt-sw-5-m",
		At: 1_790_000_500, Acked: false,
	}), "set stack failover")

	// Three deployable stacks with a current revision pointer each: the stack
	// LIST the operator wants preserved (name, repo, deploy file).
	for i, name := range []string{"test2-lms", "adam-world", "donation"} {
		must(st.RecordDeploy(ctx, &store.StackRevision{
			StackName:    name,
			Revision:     int64(1000 + i),
			SourceYAML:   "app: " + name + "\nservices:\n  web:\n    image: nginx\n",
			RenderedYAML: "version: \"3.9\"\nservices:\n  " + name + "_web:\n    image: nginx\n",
			RenderedHash: "hash-" + name,
			SourceFile:   "deploy/" + name + ".yaml",
		}, "https://github.com/example/"+name+".git"), "record deploy "+name)
	}
}

// seedExcluded fills every table that must stay out of the kit, so their
// absence in the packed database is evidence rather than an empty-table
// artifact.
func seedExcluded(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreateBackup(ctx, "", 0); err != nil {
		t.Fatalf("create backup row: %v", err)
	}
	if err := st.RecordWebhookDelivery(ctx, &store.WebhookDelivery{
		Source: "github", Status: "accepted", StackName: "test2-lms", Revision: 1000,
		RepoURL: "https://github.com/example/test2-lms.git", File: "deploy/test2-lms.yaml",
	}); err != nil {
		t.Fatalf("record webhook delivery: %v", err)
	}
	if err := st.SetStackStatus(ctx, store.StackStatus{
		StackName: "test2-lms", Status: "healthy",
		Services: map[string]string{"test2-lms_web": "healthy"}, UpdatedAt: 1_790_000_000,
	}); err != nil {
		t.Fatalf("set stack status: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO node_stats (hostname, role, cores, load1, docker_up) VALUES ('nxt-sw-4-m', 'manager', 4, 0.4, 1)`,
	); err != nil {
		t.Fatalf("insert node stats: %v", err)
	}
}

// seedRevisions fills one stack with n revisions whose payload is
// incompressible, so the "with revisions" comparison cannot be explained away
// by gzip finding repetition.
func seedRevisions(t *testing.T, st *store.Store, stack string, n int) {
	t.Helper()
	if n <= 0 {
		return
	}
	ctx := context.Background()
	now := time.Now().Unix()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO stacks (name, current_revision, repo_url, source_file, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		stack, int64(n), "https://github.com/example/bulk-app.git", "deploy/bulk-app.yaml", now, now,
	); err != nil {
		t.Fatalf("insert bulk stack: %v", err)
	}
	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin revision seed: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	seed := sha256.Sum256([]byte(stack))
	// h carries the chain ACROSS revisions, so no two revisions share a
	// payload: identical blobs would be compressed away as one dictionary
	// entry and the "with revisions" kit would look small for the wrong
	// reason.
	h := seed
	for i := 1; i <= n; i++ {
		// Distinct, effectively incompressible payload per revision: a chain of
		// sha256 digests, base64'd. Real manifests are repetitive and gzip
		// shrinks them enormously, which would let the "with revisions" half of
		// this test pass on a fixture that does not resemble a grown data.db.
		buf := make([]byte, 0, 1024)
		for len(buf) < 1024 {
			h = sha256.Sum256(h[:])
			buf = append(buf, h[:]...)
		}
		blob := base64.StdEncoding.EncodeToString(buf)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stack_revisions
			   (stack_name, revision, source_yaml, rendered_yaml, rendered_hash, source_file, payload_json, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			stack, i, blob, blob, hex.EncodeToString(h[:]), "deploy/bulk-app.yaml", `{"app":"bulk-app"}`, now,
		); err != nil {
			t.Fatalf("insert revision %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit revision seed: %v", err)
	}
}

// snapshotPayload runs one Snapshot against an in-memory Docker and returns the
// state config's bytes together with the kit that produced it.
func snapshotPayload(t *testing.T, dataDir string) (payload []byte, k *Kit, f *fakeClient) {
	t.Helper()
	f = newFake()
	k = &Kit{Docker: f, DataDir: dataDir, Log: zerolog.Nop()}
	if err := k.Snapshot(context.Background()); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	names := stateNames(t, f)
	if len(names) != 1 {
		t.Fatalf("expected 1 state config, got %v", names)
	}
	inspect, err := f.ConfigInspect(context.Background(), names[0])
	if err != nil {
		t.Fatal(err)
	}
	return inspect.Data, k, f
}

// packedDB extracts data.db from a kit payload into a scratch dir and opens it
// read-only.
func packedDB(t *testing.T, payload []byte) (*sql.DB, string) {
	t.Helper()
	entries := tarEntries(t, payload)
	blob, ok := entries["data.db"]
	if !ok {
		t.Fatalf("kit carries no data.db (entries: %v)", keys(entries))
	}
	path := filepath.Join(t.TempDir(), "packed.db")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func rowCount(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestKitDB_EveryTableIsClassified holds the schema to the table list in
// kitdb.go. A migration that adds a table without a decision fails here, which
// is the only thing standing between a future table and a promoted leader that
// silently lacks it.
func TestKitDB_EveryTableIsClassified(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	rows, err := st.DB().Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	included := map[string]bool{}
	for _, plan := range kitTables {
		if included[plan.Table] {
			t.Errorf("table %q listed twice in kitTables", plan.Table)
		}
		included[plan.Table] = true
	}
	// Created by the migration runner and by SQLite itself — never listed.
	structural := map[string]bool{"schema_version": true, "sqlite_sequence": true}

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
		if reason, excluded := excludedTables[name]; excluded && reason == "" {
			t.Errorf("table %q is excluded without a reason", name)
		}
		if included[name] && excludedTables[name] != "" {
			t.Errorf("table %q is both included and excluded", name)
		}
		if !included[name] && !structural[name] {
			if _, ok := excludedTables[name]; !ok {
				t.Errorf("table %q is neither in kitTables nor in excludedTables — decide whether a promoted leader needs it", name)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(tables)
	if len(tables) < 10 {
		t.Fatalf("expected the full schema, found %d tables: %v", len(tables), tables)
	}
}

// everyRowPlans returns the kit's table list with every history cap removed:
// the "pack the history too" comparison the size test uses as its teeth. It is
// built from kitTables so a table added to the kit is automatically included.
//
// CopyReplace survives the stripping, because it is not a cap: cluster_version's
// destination row is seeded by the migrations, so dropping the selector would make
// this comparison fail on a primary-key collision instead of measuring size.
func everyRowPlans() []store.CopyPlan {
	plans := make([]store.CopyPlan, 0, len(kitTables))
	for _, c := range kitTables {
		plan := store.CopyPlan{Table: c.Table}
		if c.Selector == store.CopyReplace {
			plan.Selector = store.CopyReplace
		}
		plans = append(plans, plan)
	}
	return plans
}

// TestSnapshot_KitFitsLimitWithTenThousandRevisions is the regression the live
// outage asked for: a leader whose database is dominated by deploy history must
// still produce a publishable kit, with the rollback window applied rather than
// by carrying the history or by dropping it. The same assertions must FAIL when
// every revision is packed, so the test cannot pass by accident.
func TestSnapshot_KitFitsLimitWithTenThousandRevisions(t *testing.T) {
	const revisions = 10_000
	dataDir, _ := realDataDir(t, revisions)

	payload, k, _ := snapshotPayload(t, dataDir)
	if len(payload) > MaxConfigBytes {
		t.Fatalf("kit is %d bytes, over the %d-byte Docker config limit", len(payload), MaxConfigBytes)
	}
	// "Comfortably under": the live kit should sit far below the ceiling, not a
	// few bytes beneath it, or the next week of growth repeats the outage.
	if len(payload) > MaxConfigBytes/2 {
		t.Fatalf("kit is %d bytes — under the limit but not comfortably (limit %d, want <= %d)", len(payload), MaxConfigBytes, MaxConfigBytes/2)
	}
	t.Logf("kit with %d revisions on disk: %d bytes (%.1f%% of the limit)",
		revisions, len(payload), 100*float64(len(payload))/float64(MaxConfigBytes))

	// The caps are APPLIED, not the history dropped: 10,000 revisions on disk
	// arrive as the newest kitStackRevisionCap for that stack, and each of the
	// three single-revision stacks arrives whole (its revision is its current
	// one).
	db, _ := packedDB(t, payload)
	var bulk int
	if err := db.QueryRow(`SELECT COUNT(*) FROM stack_revisions WHERE stack_name = 'bulk-app'`).Scan(&bulk); err != nil {
		t.Fatalf("count bulk-app revisions: %v", err)
	}
	if bulk != kitStackRevisionCap {
		t.Errorf("packed kit carries %d revisions of bulk-app, want %d (the newest %d)", bulk, kitStackRevisionCap, kitStackRevisionCap)
	}
	if n := rowCount(t, db, "stack_revisions"); n != 3+kitStackRevisionCap {
		t.Errorf("packed kit carries %d stack_revisions rows, want %d (3 stacks x 1 + %d)", n, 3+kitStackRevisionCap, kitStackRevisionCap)
	}

	// Tables that must not cross the fleet at all.
	for _, table := range []string{"backups", "webhook_deliveries", "stack_status", "node_stats"} {
		if n := rowCount(t, db, table); n != 0 {
			t.Errorf("packed kit carries %d row(s) of %s, which must not cross the fleet", n, table)
		}
	}
	if n := rowCount(t, db, "users"); n != 2 {
		t.Errorf("packed kit carries %d users, want 2", n)
	}
	if n := rowCount(t, db, "stacks"); n != 4 { // 3 seeded + the bulk stack
		t.Errorf("packed kit carries %d stacks, want 4", n)
	}

	// Teeth: the same database, packing every revision, does not fit. If the
	// export ever went back to copying the history, the assertions above would
	// still hold for a small database — this one would not.
	withRevisions := filepath.Join(t.TempDir(), "with-revisions.db")
	if _, err := store.Export(k.DBPath(), withRevisions, everyRowPlans()); err != nil {
		t.Fatalf("export with revisions: %v", err)
	}
	bigKit, err := k.packKit(withRevisions)
	if err != nil {
		t.Fatalf("pack with-revisions kit: %v", err)
	}
	if len(bigKit) <= MaxConfigBytes {
		t.Fatalf("the with-revisions kit is %d bytes and fits the %d-byte limit — this test would not catch the regression it exists for", len(bigKit), MaxConfigBytes)
	}
	t.Logf("same fixture with every revision packed: %d bytes (%.1fx the limit)", len(bigKit), float64(len(bigKit))/float64(MaxConfigBytes))
}

// TestSnapshot_KitStaysSmallWithFullHistory is the size guard that stops the
// rollback window creeping back open. A live leader accumulates history
// forever: this fixture gives two stacks 500 revisions each and two configs 200
// versions each, then holds the packed kit under 40 % of the ceiling AND holds
// the caps to the operator's numbers (5 and 10). Either drifting — a bigger cap,
// or one that quietly stops being applied — fails here.
func TestSnapshot_KitStaysSmallWithFullHistory(t *testing.T) {
	const revisionsPerStack = 500
	const versionsPerConfig = 200

	dataDir, st := realDataDir(t, revisionsPerStack)
	// A SECOND stack with its own 500 revisions: the cap must be per stack, not
	// 500 rows for whichever stack the query happened to favour.
	seedRevisions(t, st, "bulk-two", revisionsPerStack)
	seedConfigVersions(t, st, "traefik_dynamic", versionsPerConfig)
	seedConfigVersions(t, st, "lms_env", versionsPerConfig)

	payload, _, _ := snapshotPayload(t, dataDir)

	// The budget the operator set for the caps: under 40 % of the 512,000-byte
	// Docker ceiling, with margin for a few more stacks.
	const budgetPct = 40
	if len(payload) > MaxConfigBytes*budgetPct/100 {
		t.Fatalf("kit with %d revisions/stack and %d versions/config is %d bytes (%.1f%%), over the %d%% budget (%d bytes)",
			revisionsPerStack, versionsPerConfig, len(payload),
			100*float64(len(payload))/float64(MaxConfigBytes), budgetPct, MaxConfigBytes*budgetPct/100)
	}
	t.Logf("kit with %d x %d revisions and %d x %d config versions: %d bytes (%.1f%% of the limit)",
		2, revisionsPerStack, 2, versionsPerConfig, len(payload),
		100*float64(len(payload))/float64(MaxConfigBytes))

	db, _ := packedDB(t, payload)
	for _, stack := range []string{"bulk-app", "bulk-two"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM stack_revisions WHERE stack_name = ?`, stack).Scan(&n); err != nil {
			t.Fatalf("count revisions of %s: %v", stack, err)
		}
		// 5 is kitStackRevisionCap, spelled out on purpose: it is the
		// operator's number, and a silent change to it is exactly what this
		// test exists to catch.
		if n != 5 {
			t.Errorf("kit carries %d revisions of %s, want 5 (the newest per stack)", n, stack)
		}
	}
	for _, config := range []string{"traefik_dynamic", "lms_env"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM config_versions v JOIN configs c ON c.id = v.config_id WHERE c.name = ?`, config).Scan(&n); err != nil {
			t.Fatalf("count versions of %s: %v", config, err)
		}
		if n != 10 {
			t.Errorf("kit carries %d versions of %s, want 10 (the newest per config name)", n, config)
		}
	}

	// And the history is still on the leader: the caps are a copy policy, never
	// a delete. 2 stacks x 500 + the 3 single-revision stacks.
	if n := rowCount(t, st.DB(), "stack_revisions"); n != 2*revisionsPerStack+3 {
		t.Errorf("leader's own stack_revisions has %d rows, want %d", n, 2*revisionsPerStack+3)
	}
}

// TestKitDB_AuthoritativeRowsSurviveRoundTrip proves fidelity table by table:
// every authoritative row crosses a pack → unpack, values included — credential
// ciphertexts, webhook secrets, token hashes, settings and the stack list.
func TestKitDB_AuthoritativeRowsSurviveRoundTrip(t *testing.T) {
	ctx := context.Background()
	dataDir, src := realDataDir(t, 50)

	payload, k, _ := snapshotPayload(t, dataDir)

	// Unpack as a promoted node would.
	restored := t.TempDir()
	if _, err := k.extractKitTo(restored, payload, time.Now()); err != nil {
		t.Fatalf("extract kit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(restored, ".encryption_key"), []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	dst, err := store.Open(filepath.Join(restored, "data.db"))
	if err != nil {
		t.Fatalf("open restored DB through the daemon's store: %v", err)
	}
	defer func() { _ = dst.Close() }()

	// Row counts: every verbatim table arrives whole. The two history tables
	// arrive as a rollback window, asserted with their counts below.
	for _, plan := range kitTables {
		if plan.Selector != store.CopyAll {
			continue
		}
		want := rowCount(t, src.DB(), plan.Table)
		got := rowCount(t, dst.DB(), plan.Table)
		if got != want {
			t.Errorf("%s: %d rows restored, want %d", plan.Table, got, want)
		}
	}
	// config_versions: the fixture holds one version of traefik_dynamic, under
	// the cap, so the window is the whole history here.
	if n := rowCount(t, dst.DB(), "config_versions"); n != rowCount(t, src.DB(), "config_versions") {
		t.Errorf("config_versions: %d restored, want %d", n, rowCount(t, src.DB(), "config_versions"))
	}

	// Users: every name, token hash, role, scope and token_id index survives.
	srcUsers := queryStrings(t, src.DB(), `SELECT name||'|'||token_hash||'|'||role||'|'||stack||'|'||COALESCE(token_id,'') FROM users ORDER BY name`)
	dstUsers := queryStrings(t, dst.DB(), `SELECT name||'|'||token_hash||'|'||role||'|'||stack||'|'||COALESCE(token_id,'') FROM users ORDER BY name`)
	if srcUsers != dstUsers {
		t.Errorf("users differ after the round trip:\n--- source ---\n%s\n--- restored ---\n%s", srcUsers, dstUsers)
	}
	if srcUsers == "" {
		t.Fatal("no users in the source database — the fixture is empty")
	}

	// Credential ciphertext, byte for byte.
	cred, err := dst.Credential(ctx, "traefik_dashboard")
	if err != nil {
		t.Fatalf("restored credential: %v", err)
	}
	if !bytes.Equal(cred.PasswordCiphertext, testCredCipher) {
		t.Errorf("credential ciphertext changed: got %x want %x", cred.PasswordCiphertext, testCredCipher)
	}
	if cred.SwarmSecretName != "traefik_dashboard_v1" || cred.Username != "admin" || cred.Kind != "traefik" {
		t.Errorf("credential metadata changed: %+v", cred)
	}

	// Webhook HMAC secret and registry password.
	wh, err := dst.WebhookSource(ctx, "github")
	if err != nil {
		t.Fatalf("restored webhook source: %v", err)
	}
	if !bytes.Equal(wh.SecretCiphertext, testWebhookSecret) {
		t.Errorf("webhook secret changed: got %x want %x", wh.SecretCiphertext, testWebhookSecret)
	}
	reg, err := dst.Registry(ctx, "ghcr.io")
	if err != nil {
		t.Fatalf("restored registry: %v", err)
	}
	if !bytes.Equal(reg.PasswordCiphertext, testRegistryCiph) {
		t.Errorf("registry ciphertext changed: got %x want %x", reg.PasswordCiphertext, testRegistryCiph)
	}

	// Settings.
	for k, want := range map[string]string{"domain": "nextrum-sy.com", "tls_mode": "acme", "volume_root": "/var/stack"} {
		got, err := dst.Setting(ctx, k)
		if err != nil || got != want {
			t.Errorf("setting %s = %q (%v), want %q", k, got, err, want)
		}
	}

	// Configs, their rendered fields, and the kept version history.
	cfg, err := dst.Config(ctx, "traefik_dynamic")
	if err != nil {
		t.Fatalf("restored config: %v", err)
	}
	if cfg.Content != "# template v2\n" || cfg.Hash == "" {
		t.Errorf("config content/hash not preserved: %+v", cfg)
	}
	versions, err := dst.ListConfigVersions(ctx, "traefik_dynamic")
	if err != nil {
		t.Fatalf("list restored config versions: %v", err)
	}
	if len(versions) != 1 {
		t.Errorf("config_versions: %d restored, want 1", len(versions))
	}

	// Secrets: ciphertext + hash.
	sec, err := dst.Secret(ctx, "oo_admin_password")
	if err != nil {
		t.Fatalf("restored secret: %v", err)
	}
	if !bytes.Equal(sec.Payload, []byte{0xde, 0xad, 0x00, 0xbe, 0xef}) || sec.Hash != "sha256-of-plaintext" {
		t.Errorf("secret payload/hash changed: %+v", sec)
	}

	// Site cert metadata.
	cert, err := dst.SiteCert(ctx, "nextrum-sy.com")
	if err != nil {
		t.Fatalf("restored site cert: %v", err)
	}
	if cert.CertSecret != "cert_v3" || cert.KeySecret != "key_v3" || !cert.NotAfter.Equal(time.Unix(1_820_000_000, 0)) {
		t.Errorf("site cert metadata changed: %+v", cert)
	}

	// Failover marker.
	fo, err := dst.StackFailover(ctx, "test2-lms")
	if err != nil {
		t.Fatalf("restored failover marker: %v", err)
	}
	if fo.FromNode != "nxt-sw-4-m" || fo.ToNode != "nxt-sw-5-m" || fo.Acked {
		t.Errorf("failover marker changed: %+v", fo)
	}

	// The stack LIST: names, repos, deploy files, current revision pointers.
	stacks, err := dst.ListStacks(ctx)
	if err != nil {
		t.Fatalf("list restored stacks: %v", err)
	}
	if len(stacks) != 4 {
		t.Fatalf("restored %d stacks, want 4", len(stacks))
	}
	byName := map[string]*store.Stack{}
	for _, s := range stacks {
		byName[s.Name] = s
	}
	lms, ok := byName["test2-lms"]
	if !ok {
		t.Fatal("test2-lms missing from the restored stack list")
	}
	if lms.RepoURL.String != "https://github.com/example/test2-lms.git" || lms.SourceFile != "deploy/test2-lms.yaml" {
		t.Errorf("stack provenance lost: %+v", lms)
	}
	// current_revision is kept as the pointer it is, and the row behind it
	// crossed too — that row is the rendered compose reconcile and rollback
	// need, and its fidelity is asserted here rather than assumed.
	if lms.CurrentRevision != 1000 {
		t.Errorf("current_revision = %d, want 1000", lms.CurrentRevision)
	}
	wantRev, err := src.Revision(ctx, lms.Name, lms.CurrentRevision)
	if err != nil {
		t.Fatalf("source revision %d: %v", lms.CurrentRevision, err)
	}
	gotRev, err := dst.Revision(ctx, lms.Name, lms.CurrentRevision)
	if err != nil {
		t.Fatalf("restored kit has the stack pointer but not the revision row behind it (%d): %v", lms.CurrentRevision, err)
	}
	if gotRev.RenderedYAML != wantRev.RenderedYAML || gotRev.SourceYAML != wantRev.SourceYAML {
		t.Errorf("restored revision %d changed:\n got %+v\nwant %+v", lms.CurrentRevision, gotRev, wantRev)
	}
	if gotRev.RenderedHash != wantRev.RenderedHash || gotRev.SourceFile != wantRev.SourceFile {
		t.Errorf("restored revision %d lost its hash/source file: %+v", lms.CurrentRevision, gotRev)
	}
}

// seedConfigVersions appends n history rows to a config, the way the console
// does (one row per edit, newest id last).
func seedConfigVersions(t *testing.T, st *store.Store, config string, n int) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin config version seed: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	for i := 1; i <= n; i++ {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO config_versions (config_id, content, hash, created_at)
			 SELECT id, ?, ?, ? FROM configs WHERE name = ?`,
			fmt.Sprintf("# version %d of %s\n", i, config), fmt.Sprintf("hash-%d", i),
			int64(1_790_000_000+i), config,
		); err != nil {
			t.Fatalf("insert config version %d of %s: %v", i, config, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit config version seed: %v", err)
	}
}

// TestKitDB_PromotedNodeReadsCurrentRevision is the property the operator asked
// for: the row current_revision points at crosses the fleet, so a promoted
// leader can reconcile against the live manifest and roll the deploy back — and
// the revisions beyond the window do not, so the failure they produce is
// ErrRevisionNotFound, an absent row, not a broken database.
func TestKitDB_PromotedNodeReadsCurrentRevision(t *testing.T) {
	ctx := context.Background()
	// 20 revisions of bulk-app on the source, one each of the other three.
	dataDir, src := realDataDir(t, 20)
	payload, k, _ := snapshotPayload(t, dataDir)

	restored := t.TempDir()
	if _, err := k.extractKitTo(restored, payload, time.Now()); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(restored, "data.db"))
	if err != nil {
		t.Fatalf("open promoted DB: %v", err)
	}
	defer func() { _ = st.Close() }()

	row, err := st.Stack(ctx, "bulk-app")
	if err != nil {
		t.Fatalf("stack row unreadable in the kit: %v", err)
	}
	if row.CurrentRevision != 20 {
		t.Fatalf("current_revision = %d, want 20", row.CurrentRevision)
	}

	// The current revision is readable, with the rendered compose the drift
	// check and rollback consume.
	want, err := src.Revision(ctx, "bulk-app", 20)
	if err != nil {
		t.Fatalf("source revision 20: %v", err)
	}
	got, err := st.Revision(ctx, "bulk-app", 20)
	if err != nil {
		t.Fatalf("the kit has the stack pointer but not the revision behind it: %v", err)
	}
	if got.RenderedYAML != want.RenderedYAML || got.SourceYAML != want.SourceYAML || got.RenderedHash != want.RenderedHash {
		t.Errorf("current revision changed crossing the fleet:\n got %+v\nwant %+v", got, want)
	}

	// Ten of the twenty history rows are outside the window: asking for one is
	// an absent row, classified as such.
	revs, err := st.ListRevisions(ctx, "bulk-app", 0)
	if err != nil {
		t.Fatalf("list revisions of the promoted stack: %v", err)
	}
	if len(revs) != kitStackRevisionCap {
		t.Errorf("kit carries %d revisions of bulk-app, want %d", len(revs), kitStackRevisionCap)
	}
	for _, r := range revs {
		if r.Revision <= 20-kitStackRevisionCap {
			t.Errorf("kit carries revision %d, older than the newest-%d window", r.Revision, kitStackRevisionCap)
		}
	}
	if _, err := st.Revision(ctx, "bulk-app", 15); !errors.Is(err, store.ErrRevisionNotFound) {
		t.Errorf("Revision(15) = %v, want ErrRevisionNotFound (outside the window)", err)
	}

	// The rest of the control plane is unaffected.
	if users, err := st.CountUsers(ctx); err != nil || users != 2 {
		t.Fatalf("CountUsers = %d, %v; want 2", users, err)
	}
}

// TestKitDB_CurrentRevisionKeptWhenOlderThanWindow is the rollback case: a
// stack's current_revision can point at a row that is older than the newest
// five (a rollback re-points the pointer and leaves the newer rows in history).
// "The newest N" is therefore not enough — the current row must be added to the
// window explicitly, or exactly the manifest a rollback needs is the one that
// does not cross.
func TestKitDB_CurrentRevisionKeptWhenOlderThanWindow(t *testing.T) {
	ctx := context.Background()
	// Eight revisions, then a rollback: current_revision moves from 8 to 3.
	dataDir, src := realDataDir(t, 8)
	if _, err := src.DB().ExecContext(ctx,
		`UPDATE stacks SET current_revision = 3 WHERE name = 'bulk-app'`); err != nil {
		t.Fatalf("simulate rollback: %v", err)
	}
	// The small case from the brief, for completeness: three revisions with the
	// current one in the MIDDLE, so a copy that took "the newest" would keep 3
	// and drop the live one — trivially inside the cap, so it is the pointer
	// rule being asserted, not the window size.
	seedRevisions(t, src, "mid-current", 3)
	if _, err := src.DB().ExecContext(ctx,
		`UPDATE stacks SET current_revision = 2 WHERE name = 'mid-current'`); err != nil {
		t.Fatalf("simulate mid-current stack: %v", err)
	}

	payload, k, _ := snapshotPayload(t, dataDir)
	db, _ := packedDB(t, payload)

	// Newest five (4..8) plus the current row (3).
	const wantRows = kitStackRevisionCap + 1
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM stack_revisions WHERE stack_name = 'bulk-app'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != wantRows {
		t.Errorf("kit carries %d revisions of the rolled-back stack, want %d (newest %d + current)", n, wantRows, kitStackRevisionCap)
	}

	want, err := src.Revision(ctx, "bulk-app", 3)
	if err != nil {
		t.Fatalf("source revision 3: %v", err)
	}
	restored := t.TempDir()
	if _, err := k.extractKitTo(restored, payload, time.Now()); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(restored, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	row, err := st.Stack(ctx, "bulk-app")
	if err != nil {
		t.Fatal(err)
	}
	if row.CurrentRevision != 3 {
		t.Fatalf("current_revision = %d, want 3", row.CurrentRevision)
	}
	got, err := st.Revision(ctx, "bulk-app", row.CurrentRevision)
	if err != nil {
		t.Fatalf("the current revision of a rolled-back stack did not cross: %v", err)
	}
	if got.RenderedYAML != want.RenderedYAML {
		t.Errorf("current revision of a rolled-back stack changed: got %q want %q", got.RenderedYAML, want.RenderedYAML)
	}
	// The newest row crossed too (it is inside the window), so the leader can
	// still see what was deployed before the rollback.
	if _, err := st.Revision(ctx, "bulk-app", 8); err != nil {
		t.Errorf("revision 8 (inside the newest-%d window) is missing: %v", kitStackRevisionCap, err)
	}
	// The rows below the window and below current are gone.
	for _, rev := range []int64{1, 2} {
		if _, err := st.Revision(ctx, "bulk-app", rev); !errors.Is(err, store.ErrRevisionNotFound) {
			t.Errorf("Revision(%d) = %v, want ErrRevisionNotFound", rev, err)
		}
	}

	// Three revisions, current in the middle: all three cross (inside the cap)
	// and the middle one — the live manifest — is readable with its content.
	// Total: the rolled-back stack, this one, and the fixture's three
	// single-revision stacks.
	if n := rowCount(t, db, "stack_revisions"); n != wantRows+3+3 {
		t.Errorf("kit carries %d stack_revisions rows, want %d (rolled-back + mid-current + 3 single-revision stacks)", n, wantRows+3+3)
	}
	mid, err := st.Revision(ctx, "mid-current", 2)
	if err != nil {
		t.Fatalf("the middle revision of a three-revision stack did not cross: %v", err)
	}
	if mid.RenderedYAML == "" {
		t.Error("middle revision arrived without its rendered manifest")
	}
}

// TestKitDB_ConfigVersionsCappedPerConfig asserts the other half of the
// operator's window: ten versions per config NAME, not ten rows in total, and
// nothing removed from the leader's own table.
func TestKitDB_ConfigVersionsCappedPerConfig(t *testing.T) {
	dataDir, src := realDataDir(t, 3)
	seedConfigVersions(t, src, "traefik_dynamic", 200) // cluster scope
	seedConfigVersions(t, src, "lms_env", 3)           // service scope, under the cap

	payload, _, _ := snapshotPayload(t, dataDir)
	db, _ := packedDB(t, payload)

	var capped, small int
	if err := db.QueryRow(`SELECT COUNT(*) FROM config_versions v JOIN configs c ON c.id = v.config_id WHERE c.name = 'traefik_dynamic'`).Scan(&capped); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM config_versions v JOIN configs c ON c.id = v.config_id WHERE c.name = 'lms_env'`).Scan(&small); err != nil {
		t.Fatal(err)
	}
	if capped != kitConfigVersionCap {
		t.Errorf("kit carries %d versions of traefik_dynamic, want %d (the newest per config name)", capped, kitConfigVersionCap)
	}
	if small != 3 {
		t.Errorf("kit carries %d versions of lms_env, want 3 (under the cap, so all of them)", small)
	}

	// The window is the NEWEST ten: version contents are numbered, so the
	// oldest survivors must be the tail of the 200, not its head.
	var oldest string
	if err := db.QueryRow(`SELECT v.content FROM config_versions v JOIN configs c ON c.id = v.config_id
		WHERE c.name = 'traefik_dynamic' ORDER BY v.id ASC LIMIT 1`).Scan(&oldest); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(oldest, "# version 191 of traefik_dynamic") {
		t.Errorf("oldest kept version is %q, want version 191 (the ten newest of 200)", strings.TrimSpace(oldest))
	}

	// Nothing was destroyed locally: the leader keeps the whole table, so a
	// rollback further back than the window is still a leader-side operation.
	if n := rowCount(t, src.DB(), "config_versions"); n != 201+3 {
		t.Errorf("leader's own config_versions has %d rows, want 204 — the cap must not delete from the source", n)
	}
}

// TestSnapshot_LogsPayloadSizeOnEveryPublish is the observability half: the
// payload size is logged at info level whenever a kit is packed, so growth is
// visible in the log before it becomes a refused snapshot. The value logged must
// be the size actually published.
func TestSnapshot_LogsPayloadSizeOnEveryPublish(t *testing.T) {
	dataDir, _ := realDataDir(t, 5)

	var buf bytes.Buffer
	f := newFake()
	k := &Kit{Docker: f, DataDir: dataDir, Log: zerolog.New(&buf)}
	if err := k.Snapshot(context.Background()); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	names := stateNames(t, f)
	if len(names) != 1 {
		t.Fatalf("expected 1 state config, got %v", names)
	}
	inspect, err := f.ConfigInspect(context.Background(), names[0])
	if err != nil {
		t.Fatal(err)
	}

	type event struct {
		Level  string `json:"level"`
		Msg    string `json:"message"`
		Bytes  int    `json:"bytes"`
		Limit  int    `json:"limit-bytes"`
		Config string `json:"config"`
	}
	var packed *event
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("log line is not JSON: %v (%q)", err, line)
		}
		if e.Msg == "control-plane kit packed" {
			packed = &e
		}
	}
	if packed == nil {
		t.Fatalf("no info-level payload-size event was logged; log was:\n%s", buf.String())
	}
	if packed.Level != "info" {
		t.Errorf("payload-size event logged at level %q, want info", packed.Level)
	}
	if packed.Bytes != len(inspect.Data) {
		t.Errorf("logged payload size %d != published size %d", packed.Bytes, len(inspect.Data))
	}
	if packed.Limit != MaxConfigBytes {
		t.Errorf("logged limit %d, want %d", packed.Limit, MaxConfigBytes)
	}
	if packed.Bytes == 0 {
		t.Error("logged payload size is zero")
	}
}

// queryStrings returns "\n"-joined rows of a single-column query, for
// order-stable comparisons between two databases.
func queryStrings(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, "\n")
}

// slimClusterKit publishes a kit built from a real, seeded control-plane
// database (the fixture above) into the in-memory Docker fake, and returns the
// fake plus the snapshot time. 500 revisions is enough to make the kit
// "slim": the caller is testing adoption, not size (that is the 10 000-revision
// test).
func slimClusterKit(t *testing.T) (*countingDocker, time.Time) {
	t.Helper()
	ctx := context.Background()
	dir, _ := realDataDir(t, 500)
	dc := newCountingDocker()
	src := &Kit{Docker: dc, DataDir: dir, Log: zerolog.Nop()}
	if err := src.Snapshot(ctx); err != nil {
		t.Fatalf("publish slim kit: %v", err)
	}
	names, err := dc.ConfigList(ctx, LabelKind, KindState)
	if err != nil || len(names) != 1 {
		t.Fatalf("expected exactly one state config, got %v (%v)", names, err)
	}
	inspect, err := dc.ConfigInspect(ctx, names[0])
	if err != nil {
		t.Fatal(err)
	}
	at, err := parseSnapshotAt(inspect.Labels)
	if err != nil {
		t.Fatal(err)
	}
	dc.stateLists = 0
	return dc, at
}

// TestAdoptOnLeadership_SlimKitServesTheSameControlPlane is the promotion test:
// a manager that gains leadership adopts a kit with no revision history and
// then serves the SAME users (and token hashes), settings, credentials and
// stack list through the *store.Store handle every consumer already holds. A
// missing revision for a stack it can list is a normal, non-fatal state.
func TestAdoptOnLeadership_SlimKitServesTheSameControlPlane(t *testing.T) {
	ctx := context.Background()
	dc, at := slimClusterKit(t)
	dir, st := staleNode(t)
	defer func() { _ = st.Close() }()

	k := &Kit{Docker: dc, DataDir: dir, Log: zerolog.Nop()}
	swap := &swapRecorder{dbPath: filepath.Join(dir, "data.db"), inner: st}

	adoption, err := k.AdoptOnLeadership(ctx, swap)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !adoption.Adopted || swap.swaps != 1 {
		t.Fatalf("expected one adoption with one store swap, got adopted=%v swaps=%d", adoption.Adopted, swap.swaps)
	}
	if !adoption.Revision.Equal(at) {
		t.Fatalf("adoption revision = %v, want %v", adoption.Revision, at)
	}

	// Auth: the promoted leader answers for the cluster's keys, not its own.
	if n, err := st.CountUsers(ctx); err != nil || n != 2 {
		t.Fatalf("adopted users = %d (%v), want 2", n, err)
	}
	hashes := queryStrings(t, st.DB(), `SELECT name||'|'||token_hash||'|'||role FROM users ORDER BY name`)
	if !strings.Contains(hashes, "admin|argon2-hash-admin|admin") {
		t.Errorf("adopted token hashes/roles are wrong: %q", hashes)
	}
	if !strings.Contains(hashes, "deploy-bot|argon2-hash-bot|operator") {
		t.Errorf("the stack-scoped API key did not adopt: %q", hashes)
	}

	// Settings: the cluster's, not the node's stale local ones.
	if v, err := st.Setting(ctx, "domain"); err != nil || v != "nextrum-sy.com" {
		t.Errorf("adopted domain = %q (%v), want nextrum-sy.com", v, err)
	}
	if v, err := st.Setting(ctx, "probe"); err == nil && v == "local-stale" {
		t.Error("the node's own stale setting survived the adoption")
	}

	// Credential ciphertext, readable through the adopted handle.
	cred, err := st.Credential(ctx, "traefik_dashboard")
	if err != nil {
		t.Fatalf("adopted credential: %v", err)
	}
	if !bytes.Equal(cred.PasswordCiphertext, testCredCipher) {
		t.Errorf("adopted credential ciphertext changed: %x", cred.PasswordCiphertext)
	}

	// The stack LIST, intact — and the window of revisions behind it, including
	// the one current_revision points at, which is what a promoted leader needs
	// to reconcile or roll back a deploy without waiting for a re-deploy.
	stacks, err := st.ListStacks(ctx)
	if err != nil {
		t.Fatalf("adopted stack list: %v", err)
	}
	if len(stacks) != 4 {
		t.Fatalf("adopted %d stacks, want 4", len(stacks))
	}
	row, err := st.Stack(ctx, "test2-lms")
	if err != nil {
		t.Fatalf("adopted stack row: %v", err)
	}
	if row.RepoURL.String != "https://github.com/example/test2-lms.git" || row.SourceFile != "deploy/test2-lms.yaml" {
		t.Errorf("adopted stack provenance lost: %+v", row)
	}
	if revs, err := st.ListRevisions(ctx, row.Name, 0); err != nil {
		t.Errorf("listing revisions of an adopted stack errored: %v", err)
	} else if len(revs) != 1 {
		t.Errorf("adopted %d revisions for %s, want its current one", len(revs), row.Name)
	}
	cur, err := st.Revision(ctx, row.Name, row.CurrentRevision)
	if err != nil {
		t.Fatalf("adopted stack points at revision %d but the row did not cross: %v", row.CurrentRevision, err)
	}
	if cur.RenderedYAML == "" || cur.RenderedHash != "hash-"+row.Name {
		t.Errorf("adopted current revision lost its rendered manifest: %+v", cur)
	}
	if _, err := st.Revision(ctx, row.Name, row.CurrentRevision-1); !errors.Is(err, store.ErrRevisionNotFound) {
		t.Errorf("a revision outside the window reported as %v, want ErrRevisionNotFound", err)
	}

	// The adopted file is a fully migrated database: the reopened handle is
	// writable, so the next reconcile pass can do its job.
	if err := st.SetSetting(ctx, "probe2", "written-after-adopt"); err != nil {
		t.Fatalf("write through the reopened store: %v", err)
	}
}
