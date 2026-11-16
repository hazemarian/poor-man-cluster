package clustersnapshot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth/swarmtls"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "snapshot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newService(t *testing.T, st *store.Store) *Service {
	t.Helper()
	return &Service{Store: st, Hostname: "nxt-sw-5-m", Log: zerolog.Nop()}
}

// get sends one request carrying the certificate-derived identity a manager
// presents, and returns the recorder.
func get(t *testing.T, s *Service, role, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, Path, nil)
	req = req.WithContext(swarmtls.ContextWithNode(req.Context(),
		swarmtls.Node{ID: "id-node", Hostname: "nxt-sw-1-m", Role: role}))
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

type decoded struct {
	Version     int64                      `json:"version"`
	GeneratedAt string                     `json:"generated_at"`
	Leader      string                     `json:"leader"`
	Document    map[string]store.TableData `json:"document"`
	Tables      struct {
		Settings []store.SnapshotSetting `json:"settings"`
		Secrets  []store.SnapshotSecret  `json:"secrets"`
		Users    []store.SnapshotUser    `json:"users"`
	} `json:"tables"`
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) decoded {
	t.Helper()
	var d decoded
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, rec.Body.String())
	}
	return d
}

// TestSnapshotCarriesTheMirroredDocumentAndTheLegacyProjection pins the CLOSED set on
// both halves of the body. The endpoint is the one place a node can read cluster state
// over the network, and two failures are guarded here:
//
//   - a table that is neither mirrored nor excluded would travel silently, so the
//     document must be EXACTLY store.MirroredTables();
//   - the legacy three arrays still ride along for a follower on an older binary, and
//     they must describe the same rows as the document (one read, two shapes).
func TestSnapshotCarriesTheMirroredDocumentAndTheLegacyProjection(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.SetSetting(ctx, "domain", "example.test"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if _, err := st.CreateSecret(ctx, "cluster", "", "sso_client_secret", []byte("ciphertext"), strings.Repeat("a", 64)); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if _, err := st.CreateUser(ctx, "edge", "ab12", "argon2-hash"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	rec := get(t, newService(t, st), "manager", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	// Top-level shape: exactly these five keys. `document` is the whole mirrored
	// database; `tables` is the pre-document projection kept for older followers.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &top); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"version", "generated_at", "leader", "document", "tables"} {
		if _, ok := top[key]; !ok {
			t.Errorf("body is missing %q: %s", key, rec.Body.String())
		}
	}
	if len(top) != 5 {
		t.Errorf("body carries %d top-level keys, want 5: %s", len(top), rec.Body.String())
	}

	d := decode(t, rec)
	// Table set: exactly the mirrored tables, none missing, none extra. An excluded
	// table appearing here is the regression this test exists for.
	for _, name := range store.MirroredTables() {
		if _, ok := d.Document[name]; !ok {
			t.Errorf("the document is missing mirrored table %q", name)
		}
	}
	for name := range d.Document {
		if !store.IsMirrored(name) {
			t.Errorf("the document carries table %q, which is not in the mirrored set (and not in the excluded set either — classify it)", name)
		}
	}

	if d.Version < 1 {
		t.Errorf("version = %d, want at least 1", d.Version)
	}
	if d.Leader != "nxt-sw-5-m" {
		t.Errorf("leader = %q, want this node's hostname", d.Leader)
	}
	if _, err := time.Parse(time.RFC3339, d.GeneratedAt); err != nil {
		t.Errorf("generated_at %q is not RFC3339: %v", d.GeneratedAt, err)
	}
	if len(d.Tables.Settings) != 1 || d.Tables.Settings[0].Key != "domain" || d.Tables.Settings[0].Value != "example.test" {
		t.Errorf("settings = %+v, want the one row written", d.Tables.Settings)
	}
	if len(d.Tables.Secrets) != 1 || d.Tables.Secrets[0].Name != "sso_client_secret" {
		t.Fatalf("secrets = %+v, want the one row written", d.Tables.Secrets)
	}
	// The ciphertext travels; the plaintext never existed here. The assertion is
	// that the PAYLOAD is the stored ciphertext, byte for byte — a follower decrypts
	// it with the key it holds, so a hash-only projection would be useless.
	if got := string(d.Tables.Secrets[0].Payload); got != "ciphertext" {
		t.Errorf("secret payload = %q, want the stored ciphertext", got)
	}
	if len(d.Tables.Users) != 1 || d.Tables.Users[0].Name != "edge" || d.Tables.Users[0].TokenHash != "argon2-hash" {
		t.Errorf("users = %+v, want the one row written with its hash", d.Tables.Users)
	}
	// The two shapes must agree on the three tables they both carry.
	if got := d.Document["cluster_settings"].Rows; len(got) != 1 {
		t.Errorf("document carries %d setting row(s), want 1 — the legacy projection and the document must describe the same rows", len(got))
	}
}

// TestDocumentCarriesATableTheOldProjectionNeverDid is the point of the whole change: a
// table outside the original three (here a stack revision, the largest thing the mirror
// carries) must cross the wire, because a promoted leader has nothing else to read it
// from. Before the document this table was absent from the snapshot entirely.
func TestDocumentCarriesATableTheOldProjectionNeverDid(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	rev := &store.StackRevision{
		StackName:    "web",
		Revision:     3,
		SourceYAML:   "source-yaml",
		RenderedYAML: "rendered-yaml",
		RenderedHash: "hash",
	}
	if err := st.RecordDeploy(ctx, rev, "https://example.test/repo"); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}
	if err := st.CreateWebhookSource(ctx, "ci", "", []byte("hmac-ciphertext")); err != nil {
		t.Fatalf("CreateWebhookSource: %v", err)
	}

	d := decode(t, get(t, newService(t, st), "manager", ""))
	for _, name := range []string{"stacks", "stack_revisions", "webhook_sources"} {
		if got := len(d.Document[name].Rows); got != 1 {
			t.Errorf("document table %q carries %d row(s), want 1", name, got)
		}
	}
	// The rendered manifest behind a stack pointer is what a promoted leader reconciles
	// against; without it the stack is dead until someone re-deploys.
	revs := d.Document["stack_revisions"]
	rendered := -1
	for i, col := range revs.Columns {
		if col == "rendered_yaml" {
			rendered = i
		}
	}
	if rendered < 0 {
		t.Fatalf("stack_revisions arrived without a rendered_yaml column: %v", revs.Columns)
	}
	if got, _ := revs.Rows[0][rendered].(string); got != "rendered-yaml" {
		t.Errorf("rendered_yaml = %q, want the stored manifest", got)
	}
}

// TestUnchangedVersionIs304: the version IS the ETag, so a poll that has nothing to
// collect collects nothing. Remove the If-None-Match branch and this test fails on a
// 200 with a full body.
func TestUnchangedVersionIs304(t *testing.T) {
	st := newStore(t)
	svc := newService(t, st)

	first := get(t, svc, "manager", "")
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d: %s", first.Code, first.Body.String())
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("a 200 carried no ETag: nothing could ever revalidate")
	}

	second := get(t, svc, "manager", etag)
	if second.Code != http.StatusNotModified {
		t.Fatalf("second status = %d, want 304 (etag %s): %s", second.Code, etag, second.Body.String())
	}
	if second.Body.Len() != 0 {
		t.Errorf("a 304 carried a body: %s", second.Body.String())
	}
	if got := second.Header().Get("ETag"); got != etag {
		t.Errorf("304 etag = %q, want %q", got, etag)
	}
}

// TestAWriteInvalidatesTheETag is the other half of the pair: a 304 must never hide
// a change. A settings write bumps the version, so the OLD etag now answers 200 with
// the new row.
func TestAWriteInvalidatesTheETag(t *testing.T) {
	st := newStore(t)
	svc := newService(t, st)
	ctx := context.Background()

	first := get(t, svc, "manager", "")
	oldTag := first.Header().Get("ETag")
	oldVersion := decode(t, first).Version

	if err := st.SetSetting(ctx, "domain", "changed.test"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	second := get(t, svc, "manager", oldTag)
	if second.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after a settings write: %s", second.Code, second.Body.String())
	}
	d := decode(t, second)
	if d.Version <= oldVersion {
		t.Errorf("version = %d after a write, want > %d", d.Version, oldVersion)
	}
	if len(d.Tables.Settings) != 1 || d.Tables.Settings[0].Value != "changed.test" {
		t.Errorf("settings = %+v, want the new value", d.Tables.Settings)
	}
}

// TestGeneratedAtIsStableForAVersion: one stamp per version, by the writer. Two
// reads of the same version must report the SAME generated_at, otherwise a follower
// ageing the data against its own clock reads a fresh snapshot as stale the moment
// it arrives (the cache-report bug).
func TestGeneratedAtIsStableForAVersion(t *testing.T) {
	st := newStore(t)
	svc := newService(t, st)

	first := decode(t, get(t, svc, "manager", ""))
	second := decode(t, get(t, svc, "manager", ""))
	if first.GeneratedAt != second.GeneratedAt {
		t.Errorf("generated_at moved between two reads of version %d: %q then %q — the stamp belongs to the version, not the request",
			first.Version, first.GeneratedAt, second.GeneratedAt)
	}
}

// TestDeletedRowIsAbsentFromTheSnapshot: a full snapshot expresses a delete by
// omission, which is why it is a snapshot and not a delta. Removing the row from
// the body is the whole mechanism.
func TestDeletedRowIsAbsentFromTheSnapshot(t *testing.T) {
	st := newStore(t)
	svc := newService(t, st)
	ctx := context.Background()

	if err := st.SetSetting(ctx, "domain", "example.test"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	before := get(t, svc, "manager", "")
	if len(decode(t, before).Tables.Settings) != 1 {
		t.Fatalf("settings before delete = %+v", decode(t, before).Tables.Settings)
	}

	if err := st.DeleteSetting(ctx, "domain"); err != nil {
		t.Fatalf("DeleteSetting: %v", err)
	}
	after := get(t, svc, "manager", before.Header().Get("ETag"))
	if after.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after a delete (the version must have moved): %s", after.Code, after.Body.String())
	}
	if got := decode(t, after).Tables.Settings; len(got) != 0 {
		t.Errorf("settings after delete = %+v, want none — an absent row is how a follower learns to delete it", got)
	}
}

// TestEmptyTablesMarshalAsEmptyArrays: `[]`, never `null`. A follower reading null
// cannot tell "the leader has no rows" from "the leader did not send that table",
// and the two demand opposite actions.
func TestEmptyTablesMarshalAsEmptyArrays(t *testing.T) {
	rec := get(t, newService(t, newStore(t)), "manager", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`"settings":[]`, `"secrets":[]`, `"users":[]`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body does not carry %s: %s", want, rec.Body.String())
		}
	}
	for _, null := range []string{`"settings":null`, `"secrets":null`, `"users":null`} {
		if strings.Contains(rec.Body.String(), null) {
			t.Errorf("body carries %s, which reads as 'not sent': %s", null, rec.Body.String())
		}
	}
}

// TestSnapshotRefusesAWorker: the projection is for the node that mirrors it. A
// worker has no store, and the settings table carries secret values — so the answer
// is a 403 that names the rule rather than a body the caller cannot use.
func TestSnapshotRefusesAWorker(t *testing.T) {
	rec := get(t, newService(t, newStore(t)), "worker", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d for a worker certificate, want 403: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "managers only") {
		t.Errorf("refusal does not name the rule: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "settings") {
		t.Errorf("a refused response carried table data: %s", rec.Body.String())
	}
}

// TestSnapshotWithoutACertificateIsForbidden: the middleware is the only supported
// front door; a handler reached without it must refuse rather than serve to nobody.
func TestSnapshotWithoutACertificateIsForbidden(t *testing.T) {
	svc := newService(t, newStore(t))
	req := httptest.NewRequest(http.MethodGet, Path, nil)
	rec := httptest.NewRecorder()
	svc.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d without a certificate, want 403: %s", rec.Code, rec.Body.String())
	}
}

// TestSnapshotIsGetOnly: the channel is a pull. A POST is a client bug, and saying
// so beats a 404 that reads like a missing route.
func TestSnapshotIsGetOnly(t *testing.T) {
	svc := newService(t, newStore(t))
	req := httptest.NewRequest(http.MethodPost, Path, strings.NewReader("{}"))
	req = req.WithContext(swarmtls.ContextWithNode(req.Context(),
		swarmtls.Node{ID: "id", Hostname: "nxt-sw-1-m", Role: "manager"}))
	rec := httptest.NewRecorder()
	svc.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d for POST, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Errorf("Allow = %q, want GET", got)
	}
}

// TestSnapshotWithoutAStoreIs503: a leader with no store cannot answer, and a
// healthy-looking empty body would be worse than a refusal.
func TestSnapshotWithoutAStoreIs503(t *testing.T) {
	svc := &Service{Hostname: "nxt-sw-5-m", Log: zerolog.Nop()}
	rec := get(t, svc, "manager", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}

// TestETagNamesTheLeaderAndTheVersion: a version alone is not an identity. After a
// leadership change the new leader's counter can equal the old one while the content
// differs, so a version-only tag would answer 304 to a follower holding another
// leader's data.
func TestETagNamesTheLeaderAndTheVersion(t *testing.T) {
	st := newStore(t)
	a := &Service{Store: st, Hostname: "nxt-sw-5-m", Log: zerolog.Nop()}
	b := &Service{Store: st, Hostname: "nxt-sw-4-m", Log: zerolog.Nop()}

	first := get(t, a, "manager", "")
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d", first.Code)
	}
	tag := first.Header().Get("ETag")
	if !strings.Contains(tag, "nxt-sw-5-m") || !strings.Contains(tag, ":") {
		t.Fatalf("etag %q does not name the leader and the version", tag)
	}

	// The same store (so the same content and version), served by a different
	// leader: the follower's stored tag must NOT validate.
	if got := get(t, b, "manager", tag).Code; got != http.StatusOK {
		t.Fatalf("status = %d when another leader serves the same version, want 200 — the etag must include the leader", got)
	}
}

// TestETagMatchingFollowsTheWeakComparison: a list, a W/ prefix and `*` are all
// legal in If-None-Match. String equality alone is the bug that makes a revalidating
// client never hit.
func TestETagMatchingFollowsTheWeakComparison(t *testing.T) {
	st := newStore(t)
	svc := newService(t, st)
	tag := get(t, svc, "manager", "").Header().Get("ETag")

	for _, header := range []string{
		tag,
		"W/" + tag,
		`"other:1", ` + tag,
		"*",
	} {
		if got := get(t, svc, "manager", header).Code; got != http.StatusNotModified {
			t.Errorf("If-None-Match %q answered %d, want 304", header, got)
		}
	}
	for _, header := range []string{`"nxt-sw-5-m:999999"`, `"elsewhere:1"`} {
		if got := get(t, svc, "manager", header).Code; got != http.StatusOK {
			t.Errorf("If-None-Match %q answered %d, want 200", header, got)
		}
	}
}
