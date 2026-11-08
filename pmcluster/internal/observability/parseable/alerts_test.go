package parseable

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

func TestAlertsListsAndBounds(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/alerts" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"name":"disk-full","triggered":true},{"name":"no-backups","triggered":false}]`))
	}))
	defer s.Close()

	c, _ := New(Config{BaseURL: s.URL})
	got, err := c.Alerts(context.Background(), observability.AlertQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != observability.StatusAvailable {
		t.Fatalf("status = %v, want available", got.Status)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("rows = %d, want 1 (bounded by limit)", len(got.Rows))
	}
	if _, ok := got.Rows[0]["name"]; !ok {
		t.Fatalf("alert row missing name: %+v", got.Rows[0])
	}
}

func TestAlertsRejectsUnboundedLimit(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://parseable.invalid"})
	for _, limit := range []int{0, -1, observability.MaxAlertPageSize + 1} {
		if _, err := c.Alerts(context.Background(), observability.AlertQuery{Limit: limit}); err == nil {
			t.Errorf("limit %d accepted", limit)
		}
	}
}

func TestAlertsToleratesWrappedAndBareResponses(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"alerts":[{"name":"wrapped"}]}`))
	}))
	defer s.Close()

	c, _ := New(Config{BaseURL: s.URL})
	got, err := c.Alerts(context.Background(), observability.AlertQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(got.Rows))
	}
}

func TestAlertsRedactsFreeText(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"a","message":"the secret is hunter2 here"}]`))
	}))
	defer s.Close()

	c, _ := New(Config{BaseURL: s.URL, Redactor: observability.NewRedactor("hunter2")})
	got, err := c.Alerts(context.Background(), observability.AlertQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := got.Rows[0]["message"].(string)
	if strings.Contains(msg, "hunter2") {
		t.Fatalf("alert message not redacted: %q", msg)
	}
}

// TestAlertsDecodesUpstreamShapes is the wire contract, table-driven: a
// well-formed body becomes rows, an unrecognised extra field is carried (not
// rejected — the backend's alert objects will grow), and a body that is not a
// list of objects is an error rather than a half-decoded result.
func TestAlertsDecodesUpstreamShapes(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantRows   int
		wantErr    bool
		checkRow   func(t *testing.T, row observability.Row)
		errMessage string
	}{
		{
			name:     "bare array",
			body:     `[{"name":"disk-full"}]`,
			wantRows: 1,
			checkRow: func(t *testing.T, row observability.Row) {
				if row["name"] != "disk-full" {
					t.Fatalf("name = %v, want disk-full", row["name"])
				}
			},
		},
		{
			name:     "wrapped in alerts",
			body:     `{"alerts":[{"name":"a"},{"name":"b"}]}`,
			wantRows: 2,
		},
		{
			name:     "wrapped in data",
			body:     `{"data":[{"name":"a"}]}`,
			wantRows: 1,
		},
		{
			name:     "wrapped in records",
			body:     `{"records":[{"name":"a"}]}`,
			wantRows: 1,
		},
		{
			// The reason the adapter decodes into map[string]any: an upstream
			// field nobody knew about must not fail the read, and must survive
			// into the row so the console can show it.
			name:     "unknown and nested extra fields",
			body:     `[{"name":"a","state":"firing","unknown_scalar":7,"unknown_object":{"k":[1,2,{"deep":true}]},"labels":{"severity":"critical"}}]`,
			wantRows: 1,
			checkRow: func(t *testing.T, row observability.Row) {
				if row["state"] != "firing" {
					t.Fatalf("state = %v, want firing", row["state"])
				}
				if _, ok := row["unknown_scalar"]; !ok {
					t.Fatalf("unknown scalar field dropped: %+v", row)
				}
				obj, ok := row["unknown_object"].(map[string]any)
				if !ok {
					t.Fatalf("unknown object field not preserved as an object: %T", row["unknown_object"])
				}
				if _, ok := obj["k"]; !ok {
					t.Fatalf("nested unknown value dropped: %+v", obj)
				}
			},
		},
		{
			name:     "empty list",
			body:     `[]`,
			wantRows: 0,
		},
		{
			name:     "known envelope explicitly null is an empty list",
			body:     `{"alerts":null}`,
			wantRows: 0,
		},
		{
			name:       "row is not an object",
			body:       `[1,2,3]`,
			wantErr:    true,
			errMessage: "not an object",
		},
		{
			name:       "body is a scalar",
			body:       `42`,
			wantErr:    true,
			errMessage: "not an object or array",
		},
		{
			name:       "body is malformed json",
			body:       `not json`,
			wantErr:    true,
			errMessage: "invalid character",
		},
		{
			name:       "object without a list",
			body:       `{"unexpected":true}`,
			wantErr:    true,
			errMessage: "does not contain a list",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()

			c, err := New(Config{BaseURL: s.URL})
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.Alerts(context.Background(), observability.AlertQuery{Limit: 10})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got rows %+v", got.Rows)
				}
				if tc.errMessage != "" && !strings.Contains(err.Error(), tc.errMessage) {
					t.Fatalf("error = %q, want it to mention %q", err, tc.errMessage)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Status != observability.StatusAvailable {
				t.Fatalf("status = %v, want available", got.Status)
			}
			if len(got.Rows) != tc.wantRows {
				t.Fatalf("rows = %d, want %d", len(got.Rows), tc.wantRows)
			}
			if tc.checkRow != nil && len(got.Rows) > 0 {
				tc.checkRow(t, got.Rows[0])
			}
		})
	}
}

// TestAlertsTruncatesToTheRequestedLimit: Parseable's alerts endpoint takes no
// limit parameter, so the cap is enforced on the decoded list. Assert the rows
// that come back are the requested number, in upstream order — the caller's
// bound is honoured against a larger upstream answer.
func TestAlertsTruncatesToTheRequestedLimit(t *testing.T) {
	var requests int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		_, _ = w.Write([]byte(`[{"name":"a"},{"name":"b"},{"name":"c"},{"name":"d"},{"name":"e"}]`))
	}))
	defer s.Close()

	c, _ := New(Config{BaseURL: s.URL})
	got, err := c.Alerts(context.Background(), observability.AlertQuery{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(got.Rows))
	}
	for i, want := range []string{"a", "b", "c"} {
		if got.Rows[i]["name"] != want {
			t.Fatalf("row %d = %v, want %s (upstream order is preserved)", i, got.Rows[i]["name"], want)
		}
	}
	if n := atomic.LoadInt32(&requests); n != 1 {
		t.Fatalf("upstream requests = %d, want 1", n)
	}
}

// TestAlertsRefusesOverCapBeforeTouchingTheBackend: a limit above the cap is
// refused by AlertQuery.Validate, and the refusal must happen BEFORE any HTTP
// request — the server sees zero requests. The cap therefore cannot be widened
// by a caller and cannot cost a backend round trip either.
func TestAlertsRefusesOverCapBeforeTouchingTheBackend(t *testing.T) {
	var requests int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer s.Close()

	c, _ := New(Config{BaseURL: s.URL})
	for _, limit := range []int{observability.MaxAlertPageSize + 1, 5000, 0, -7} {
		_, err := c.Alerts(context.Background(), observability.AlertQuery{Limit: limit})
		if err == nil {
			t.Fatalf("limit %d accepted", limit)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("between 1 and %d", observability.MaxAlertPageSize)) {
			t.Errorf("limit %d error = %q, want it to name the range", limit, err)
		}
	}
	if n := atomic.LoadInt32(&requests); n != 0 {
		t.Fatalf("upstream requests = %d, want 0 (an over-cap limit must be refused locally)", n)
	}

	// The cap itself is accepted.
	if _, err := c.Alerts(context.Background(), observability.AlertQuery{Limit: observability.MaxAlertPageSize}); err != nil {
		t.Fatalf("limit = cap: %v", err)
	}
	if n := atomic.LoadInt32(&requests); n != 1 {
		t.Fatalf("upstream requests at exactly the cap = %d, want 1", n)
	}
}

// TestAlertsMapsBackendFailuresToUnavailable: a 5xx, a timeout and an
// over-sized body each become ErrUnavailable instead of a panic or a nil
// dereference, so the handler can answer its documented shape.
func TestAlertsMapsBackendFailuresToUnavailable(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		timeout time.Duration
		maxByte int64
	}{
		{
			name: "5xx",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "upstream exploded", http.StatusBadGateway)
			},
		},
		{
			name: "timeout",
			handler: func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(400 * time.Millisecond)
				_, _ = w.Write([]byte(`[]`))
			},
			timeout: 50 * time.Millisecond,
		},
		{
			name: "response over the byte cap",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`[` + strings.Repeat(`{"name":"x"},`, 200) + `{"name":"y"}]`))
			},
			maxByte: 64,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(tc.handler)
			defer s.Close()

			cfg := Config{BaseURL: s.URL, Timeout: tc.timeout, MaxBytes: tc.maxByte}
			c, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			res, err := c.Alerts(context.Background(), observability.AlertQuery{Limit: 10})
			if err == nil {
				t.Fatalf("want an error, got %+v", res)
			}
			if !errors.Is(err, observability.ErrUnavailable) {
				t.Fatalf("error = %v, want one wrapping ErrUnavailable", err)
			}
			if res.Status == observability.StatusAvailable {
				t.Fatalf("status = %v on a failed read", res.Status)
			}
			if len(res.Rows) != 0 {
				t.Fatalf("failed read returned %d rows", len(res.Rows))
			}
		})
	}
}

// TestAlertsSendsTheManagedCredential: the adapter authenticates with the
// daemon's Basic credential and asks only for the alerts list — no window, no
// SQL, no caller-controlled parameter in the URL.
func TestAlertsSendsTheManagedCredential(t *testing.T) {
	type seen struct {
		path   string
		query  string
		accept string
		auth   string
	}
	var got seen
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = seen{
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			accept: r.Header.Get("Accept"),
			auth:   r.Header.Get("Authorization"),
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer s.Close()

	c, _ := New(Config{BaseURL: s.URL, Username: "ops@example.test", Password: "s3cret"})
	if _, err := c.Alerts(context.Background(), observability.AlertQuery{Limit: observability.MaxAlertPageSize}); err != nil {
		t.Fatal(err)
	}

	if got.path != "/api/v1/alerts" {
		t.Errorf("path = %q, want /api/v1/alerts", got.path)
	}
	if got.query != "" {
		t.Errorf("query = %q, want none (the backend takes no bound)", got.query)
	}
	if got.accept != "application/json" {
		t.Errorf("Accept = %q", got.accept)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("ops@example.test:s3cret"))
	if got.auth != want {
		t.Errorf("Authorization = %q, want the managed Basic credential", got.auth)
	}
}

// TestAlertsRedactsSecretBearingFields: a secret in free text AND in a
// secret-named field is masked, while the rest of the alert stays intact.
func TestAlertsRedactsSecretBearingFields(t *testing.T) {
	body := `[{"name":"disk-full","state":"firing","message":"auth failed for token abcdef123456","labels":{"severity":"critical"},"token":"abcdef123456","password":"hunter2"}]`
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer s.Close()

	c, _ := New(Config{BaseURL: s.URL, Redactor: observability.NewRedactor("abcdef123456", "hunter2")})
	got, err := c.Alerts(context.Background(), observability.AlertQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(got.Rows))
	}
	row := got.Rows[0]

	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"abcdef123456", "hunter2"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("secret %q survived redaction: %s", secret, encoded)
		}
	}
	// The fields an operator needs are still there.
	if row["name"] != "disk-full" || row["state"] != "firing" {
		t.Fatalf("redaction damaged the alert: %+v", row)
	}
	labels, ok := row["labels"].(map[string]any)
	if !ok || labels["severity"] != "critical" {
		t.Fatalf("labels lost: %+v", row["labels"])
	}
}
