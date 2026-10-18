package parseable

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

func queryFixture() observability.Query {
	end := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	return observability.Query{Service: "demo_web", Window: observability.Window{Start: end.Add(-time.Hour), End: end}, Limit: 2}
}

func TestQueryContractAndBounds(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v1/query" || r.Header.Get("Authorization") != "Basic fixture" {
			t.Error("request contract")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["startTime"] != "2026-10-17T11:00:00Z" || body["endTime"] != "2026-10-17T12:00:00Z" {
			t.Errorf("bounds: %v", body)
		}
		if !strings.Contains(body["query"], "`service.name` = 'demo_web'") || !strings.HasSuffix(body["query"], "LIMIT 2") {
			t.Error(body["query"])
		}
		fmt.Fprint(w, `[{"body":"one"},{"body":"two"},{"body":"backend ignored limit"}]`)
	}))
	defer s.Close()
	c, err := New(Config{BaseURL: s.URL, AuthHeader: "Basic fixture"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Query(context.Background(), queryFixture())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 2 || !got.Truncated {
		t.Fatalf("unbounded: %+v", got)
	}
}

func TestQueryEmptyMalformedAndOversized(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		max        int64
		bad        bool
	}{
		{"empty", "[]", 128, false}, {"null", "null", 128, true}, {"error", `{"error":"no"}`, 128, true},
		{"nested", `{"data":{"error":"no"}}`, 128, true}, {"trailing", `[] {}`, 128, true},
		{"oversized", `[{"body":"` + strings.Repeat("a", 200) + `"}]`, 128, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }))
			defer s.Close()
			c, _ := New(Config{BaseURL: s.URL, MaxBytes: tc.max})
			got, err := c.Query(context.Background(), queryFixture())
			if (err != nil) != tc.bad {
				t.Fatalf("err=%v", err)
			}
			if !tc.bad && (len(got.Rows) != 0 || got.Status != observability.StatusAvailable) {
				t.Fatalf("empty not available: %+v", got)
			}
		})
	}
}

func TestQueryRejectsBusyCapacity(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; fmt.Fprint(w, "[]") }))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, MaxConcurrent: 1})
	done := make(chan error, 1)
	go func() { _, err := c.Query(context.Background(), queryFixture()); done <- err }()
	<-entered
	_, err := c.Query(context.Background(), queryFixture())
	close(release)
	if err == nil {
		t.Fatal("busy source accepted request")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
