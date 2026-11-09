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

// TestKitDB_EveryTableIsClassified holds the schema to the table lists in
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
	for _, name := range authoritativeTables {
		if included[name] {
			t.Errorf("table %q listed twice in authoritativeTables", name)
		}
		included[name] = true
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
				t.Errorf("table %q is neither in authoritativeTables nor in excludedTables — decide whether a promoted leader needs it", name)
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

// TestSnapshot_SlimKitFitsLimitWithTenThousandRevisions is the regression the
// live outage asked for: the leader's database dominated by deploy history must
// still produce a publishable kit — and the same assertion must FAIL when the
// revision history is packed, so the test cannot pass by accident.
func TestSnapshot_SlimKitFitsLimitWithTenThousandRevisions(t *testing.T) {
	const revisions = 10_000
	dataDir, _ := realDataDir(t, revisions)

	payload, k, _ := snapshotPayload(t, dataDir)
	if len(payload) > MaxConfigBytes {
		t.Fatalf("slim kit is %d bytes, over the %d-byte Docker config limit", len(payload), MaxConfigBytes)
	}
	// "Comfortably under": the live kit should sit far below the ceiling, not a
	// few bytes beneath it, or the next week of growth repeats the outage.
	if len(payload) > MaxConfigBytes/2 {
		t.Fatalf("slim kit is %d bytes — under the limit but not comfortably (limit %d, want <= %d)", len(payload), MaxConfigBytes, MaxConfigBytes/2)
	}
	t.Logf("slim kit with %d revisions on disk: %d bytes (%.1f%% of the limit)",
		revisions, len(payload), 100*float64(len(payload))/float64(MaxConfigBytes))

	// The excluded tables are ABSENT from the packed database. This is the
	// assertion that gives the size number its meaning: an empty payload could
	// also have come from a packer that dropped everything.
	db, _ := packedDB(t, payload)
	for _, table := range []string{"stack_revisions", "backups", "webhook_deliveries", "stack_status", "node_stats"} {
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

	// Teeth: the same database, packing revisions too, does not fit. If the
	// export ever went back to copying everything, the assertions above would
	// still hold for a small database — this one would not.
	withRevisions := filepath.Join(t.TempDir(), "with-revisions.db")
	if _, err := store.ExportDB(k.DBPath(), withRevisions, append(append([]string{}, authoritativeTables...), "stack_revisions")); err != nil {
		t.Fatalf("export with revisions: %v", err)
	}
	bigKit, err := k.packKit(withRevisions)
	if err != nil {
		t.Fatalf("pack with-revisions kit: %v", err)
	}
	if len(bigKit) <= MaxConfigBytes {
		t.Fatalf("the with-revisions kit is %d bytes and fits the %d-byte limit — this test would not catch the regression it exists for", len(bigKit), MaxConfigBytes)
	}
	t.Logf("same fixture with revisions packed: %d bytes (%.1fx the limit)", len(bigKit), float64(len(bigKit))/float64(MaxConfigBytes))
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

	// Row counts per authoritative table, source vs restored.
	for _, table := range authoritativeTables {
		want := rowCount(t, src.DB(), table)
		got := rowCount(t, dst.DB(), table)
		if got != want {
			t.Errorf("%s: %d rows restored, want %d", table, got, want)
		}
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
	// current_revision is kept as the pointer it is; the revision row behind it
	// is deliberately absent (asserted below).
	if lms.CurrentRevision != 1000 {
		t.Errorf("current_revision = %d, want 1000", lms.CurrentRevision)
	}
}

// TestKitDB_MissingRevisionIsOrdinary checks the property the operator cares
// about downstream: a stack whose current revision row is NOT in the kit still
// reads back as a normal stack, and asking for that revision reports
// ErrRevisionNotFound — a missing row, not a broken database.
func TestKitDB_MissingRevisionIsOrdinary(t *testing.T) {
	ctx := context.Background()
	dataDir, _ := realDataDir(t, 20)
	payload, k, _ := snapshotPayload(t, dataDir)

	restored := t.TempDir()
	if _, err := k.extractKitTo(restored, payload, time.Now()); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(restored, "data.db"))
	if err != nil {
		t.Fatalf("open slim DB: %v", err)
	}
	defer func() { _ = st.Close() }()

	row, err := st.Stack(ctx, "test2-lms")
	if err != nil {
		t.Fatalf("stack row unreadable in a slim kit: %v", err)
	}
	revs, err := st.ListRevisions(ctx, "test2-lms", 10)
	if err != nil {
		t.Fatalf("listing revisions of a slim kit errored: %v", err)
	}
	if len(revs) != 0 {
		t.Fatalf("slim kit carries %d revisions for test2-lms, want 0", len(revs))
	}
	if _, err := st.Revision(ctx, row.Name, row.CurrentRevision); err == nil {
		t.Fatal("expected ErrRevisionNotFound for a revision that did not cross the fleet")
	} else if !errors.Is(err, store.ErrRevisionNotFound) {
		t.Fatalf("missing revision reported as %v, want ErrRevisionNotFound (an absent row is normal, not corruption)", err)
	}
	// The rest of the control plane is unaffected by that absence.
	if users, err := st.CountUsers(ctx); err != nil || users != 2 {
		t.Fatalf("CountUsers = %d, %v; want 2", users, err)
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

	// The stack LIST, intact — with no revision behind it, which must read as
	// "no stored revision", not as damage.
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
	if revs, err := st.ListRevisions(ctx, row.Name, 10); err != nil {
		t.Errorf("listing revisions of an adopted stack errored: %v", err)
	} else if len(revs) != 0 {
		t.Errorf("adopted %d revisions, want 0 (they do not cross the fleet)", len(revs))
	}
	if _, err := st.Revision(ctx, row.Name, row.CurrentRevision); !errors.Is(err, store.ErrRevisionNotFound) {
		t.Errorf("missing revision reported as %v, want ErrRevisionNotFound", err)
	}

	// The adopted file is a fully migrated database: the reopened handle is
	// writable, so the next reconcile pass can do its job.
	if err := st.SetSetting(ctx, "probe2", "written-after-adopt"); err != nil {
		t.Fatalf("write through the reopened store: %v", err)
	}
}
