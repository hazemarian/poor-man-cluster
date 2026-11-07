package parseable

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
