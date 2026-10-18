package observability

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactorProtectsEvidence(t *testing.T) {
	r := NewRedactor("naked-server-credential")
	quoted, _ := json.Marshal(map[string]string{"password": `first"second-secret`})
	rows := []Row{{"token": "top-level-secret", "body": map[string]any{"password": "nested-secret", "message": string(quoted) + " naked-server-credential"}}}
	got, _ := json.Marshal(r.Rows(rows))
	for _, secret := range []string{"top-level-secret", "nested-secret", "second-secret", "naked-server-credential"} {
		if strings.Contains(string(got), secret) {
			t.Fatalf("secret leaked: %s", secret)
		}
	}
	if rows[0]["token"] != "top-level-secret" {
		t.Fatal("source row mutated")
	}
	if got := r.Text("redis dial: connection refused"); got != "redis dial: connection refused" {
		t.Fatal(got)
	}
	projected := r.Rows([]Row{{"p_timestamp": "2026-10-17T02:43:43.123Z", "body": "192.0.2.1 192.0.2.2"}})
	if projected[0]["p_timestamp"] != "2026-10-17T02:43:43.123Z" || strings.Contains(projected[0]["body"].(string), "192.0.2") {
		t.Fatalf("timestamp or address redaction: %v", projected)
	}
}

func TestParseableUTCWithoutZoneIsNotPhone(t *testing.T) {
	r := Redactor{}
	rows := r.Rows([]Row{{"p_timestamp": "2026-10-17T08:44:03.765", "body": "2026-10-17"}})
	if rows[0]["p_timestamp"] != "2026-10-17T08:44:03.765" {
		t.Fatalf("timestamp redacted: %v", rows[0])
	}
	if validEvidenceTimestamp("2026-99-99T08:44:03") {
		t.Fatal("invalid date accepted")
	}
}

func TestISODatesInBodiesSurvivePhoneRedaction(t *testing.T) {
	r := Redactor{}
	got := r.Text("2026-10-18T22:29:09.745+0300 info call +1 (555) 123-4567 or 555-123-4567")
	if !strings.Contains(got, "2026-10-18T22:29:09.745") {
		t.Fatalf("date redacted: %s", got)
	}
	if strings.Contains(got, "555") || strings.Count(got, "<PHONE>") != 2 {
		t.Fatalf("phone numbers must stay redacted: %s", got)
	}
}
