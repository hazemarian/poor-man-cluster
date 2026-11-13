package clustersnapshot

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// fakeDoer is the transport seam: it records what the client asked for and answers
// with whatever the test decided.
type fakeDoer struct {
	status  int
	header  http.Header
	body    string
	err     error
	calls   int
	path    string
	method  string
	sent    map[string]string
	baseURL string
}

func (f *fakeDoer) Do(_ context.Context, method, path string, header map[string]string) (int, http.Header, []byte, string, error) {
	f.calls++
	f.method = method
	f.path = path
	f.sent = header
	if f.err != nil {
		return 0, nil, nil, "", f.err
	}
	h := f.header
	if h == nil {
		h = http.Header{}
	}
	base := f.baseURL
	if base == "" {
		base = "https://10.7.224.5:9092"
	}
	return f.status, h, []byte(f.body), base, nil
}

func okBody(leader string, version int64) string {
	return `{"version":` + itoa(version) + `,"generated_at":"2026-11-13T12:00:00Z","leader":"` + leader +
		`","tables":{"settings":[{"key":"domain","value":"example.test"}],"secrets":[],"users":[{"id":1,"name":"edge","token_id":"ab12","token_hash":"argon2","role":"admin","stack":"","created_at":1}]}}`
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestFetchDecodesTheWireShape(t *testing.T) {
	doer := &fakeDoer{status: http.StatusOK, body: okBody("nxt-sw-5-m", 7), header: http.Header{"Etag": []string{`"nxt-sw-5-m:7"`}}}
	c := &Client{Do: doer, Log: zerolog.Nop()}

	res, err := c.Fetch(context.Background(), "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.NotModified {
		t.Error("a 200 was reported as not modified")
	}
	if res.Leader != "nxt-sw-5-m" || res.Snapshot.Version != 7 {
		t.Errorf("leader/version = %q/%d, want nxt-sw-5-m/7", res.Leader, res.Snapshot.Version)
	}
	// The ETag the client keeps must be derived from the body, not copied from a header
	// a proxy could have rewritten — and it must equal what the server composes, or the
	// next poll never matches.
	if want := ETag("nxt-sw-5-m", 7); res.ETag != want {
		t.Errorf("etag = %q, want %q", res.ETag, want)
	}
	if len(res.Snapshot.Settings) != 1 || res.Snapshot.Settings[0].Key != "domain" {
		t.Errorf("settings = %+v", res.Snapshot.Settings)
	}
	if len(res.Snapshot.Users) != 1 || res.Snapshot.Users[0].Name != "edge" {
		t.Errorf("users = %+v", res.Snapshot.Users)
	}
	if got, want := res.Snapshot.GeneratedAt, time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC).Unix(); got != want {
		t.Errorf("generated_at = %d, want %d (RFC3339 parsed, not passed as a string)", got, want)
	}
	if doer.path != Path || doer.method != http.MethodGet {
		t.Errorf("request = %s %s, want GET %s", doer.method, doer.path, Path)
	}
	if _, sent := doer.sent["If-None-Match"]; sent {
		t.Error("an unconditional poll sent If-None-Match")
	}
}

func TestFetchSendsTheConditionalHeader(t *testing.T) {
	doer := &fakeDoer{status: http.StatusNotModified, header: http.Header{"Etag": []string{`"nxt-sw-5-m:7"`}}}
	c := &Client{Do: doer, Log: zerolog.Nop()}

	res, err := c.Fetch(context.Background(), ETag("nxt-sw-5-m", 7))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !res.NotModified {
		t.Fatal("a 304 was reported as a body")
	}
	if res.Snapshot != nil {
		t.Errorf("a 304 produced a snapshot: %+v", res.Snapshot)
	}
	if got := doer.sent["If-None-Match"]; got != ETag("nxt-sw-5-m", 7) {
		t.Errorf("If-None-Match = %q, want the pair this node applied", got)
	}
}

// TestFetchTreatsARefusalAsAnAnswer: 403 means the leader rejected this node's
// certificate. It is not a reason to walk to another manager, and the error has to
// carry the leader's own reason or an operator is left guessing.
func TestFetchTreatsARefusalAsAnAnswer(t *testing.T) {
	doer := &fakeDoer{status: http.StatusForbidden, body: `{"error":"forbidden","reason":"the cluster snapshot is served to managers only"}`}
	c := &Client{Do: doer, Log: zerolog.Nop()}

	_, err := c.Fetch(context.Background(), "")
	if err == nil {
		t.Fatal("a 403 was accepted")
	}
	if !strings.Contains(err.Error(), "managers only") {
		t.Errorf("error does not carry the leader's reason: %v", err)
	}
}

// TestFetchRefusesAnUnusableAnswer: a body with no leader, or a generated_at that is
// not RFC3339 (a version skew during a rolling upgrade), must fail loudly here rather
// than be applied as zeroes.
func TestFetchRefusesAnUnusableAnswer(t *testing.T) {
	t.Run("no leader", func(t *testing.T) {
		doer := &fakeDoer{status: http.StatusOK, body: `{"version":3,"generated_at":"2026-11-13T12:00:00Z","leader":"","tables":{"settings":[],"secrets":[],"users":[]}}`}
		if _, err := (&Client{Do: doer, Log: zerolog.Nop()}).Fetch(context.Background(), ""); err == nil {
			t.Fatal("a snapshot with no leader was accepted; the next ETag would be unusable")
		}
	})
	t.Run("bad timestamp", func(t *testing.T) {
		doer := &fakeDoer{status: http.StatusOK, body: `{"version":3,"generated_at":"yesterday","leader":"nxt-sw-5-m","tables":{"settings":[],"secrets":[],"users":[]}}`}
		if _, err := (&Client{Do: doer, Log: zerolog.Nop()}).Fetch(context.Background(), ""); err == nil {
			t.Fatal("a non-RFC3339 generated_at was accepted")
		}
	})
	t.Run("no transport", func(t *testing.T) {
		if _, err := (&Client{Log: zerolog.Nop()}).Fetch(context.Background(), ""); err == nil {
			t.Fatal("a client with no transport was accepted")
		}
	})
}
