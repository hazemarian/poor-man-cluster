package store

import (
	"context"
	"testing"
)

func TestCreateWebhookSource_HappyPath(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	err := s.CreateWebhookSource(ctx, "github-prod", "GitHub production webhook", []byte("encrypted-secret"))
	if err != nil {
		t.Fatalf("CreateWebhookSource: %v", err)
	}

	got, err := s.WebhookSource(ctx, "github-prod")
	if err != nil {
		t.Fatalf("WebhookSource: %v", err)
	}
	if got.Source != "github-prod" {
		t.Errorf("Source = %q, want 'github-prod'", got.Source)
	}
	if string(got.SecretCiphertext) != "encrypted-secret" {
		t.Errorf("SecretCiphertext = %q, want 'encrypted-secret'", got.SecretCiphertext)
	}
	if !got.Description.Valid {
		t.Error("Description.Valid = false, want true")
	}
	if got.Description.String != "GitHub production webhook" {
		t.Errorf("Description = %q, want 'GitHub production webhook'", got.Description.String)
	}
	if got.CreatedAt == 0 {
		t.Error("CreatedAt should be non-zero")
	}
	if got.LastUsedAt.Valid {
		t.Error("LastUsedAt should be NULL on a fresh row")
	}
}

func TestCreateWebhookSource_DuplicateSource(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.CreateWebhookSource(ctx, "github", "first", []byte("s1")); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := s.CreateWebhookSource(ctx, "github", "second", []byte("s2"))
	if err == nil {
		t.Fatal("expected ErrWebhookSourceExists, got nil")
	}
	if err != ErrWebhookSourceExists {
		t.Errorf("err = %v, want ErrWebhookSourceExists", err)
	}
}

func TestCreateWebhookSource_EmptyDescription_NullInDB(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.CreateWebhookSource(ctx, "ci", "", []byte("sec")); err != nil {
		t.Fatalf("CreateWebhookSource: %v", err)
	}

	got, err := s.WebhookSource(ctx, "ci")
	if err != nil {
		t.Fatalf("WebhookSource: %v", err)
	}
	if got.Description.Valid {
		t.Errorf("Description.Valid = true, want false (NULL) when description is empty; got %q", got.Description.String)
	}
}

func TestGetWebhookSource_UnknownSource(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, err := s.WebhookSource(ctx, "does-not-exist")
	if err == nil {
		t.Fatal("expected ErrWebhookSourceNotFound, got nil")
	}
	if err != ErrWebhookSourceNotFound {
		t.Errorf("err = %v, want ErrWebhookSourceNotFound", err)
	}
}

func TestListWebhookSources_AlphabeticalOrder(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	names := []string{"zulu", "alpha", "mike", "bravo"}
	for _, n := range names {
		if err := s.CreateWebhookSource(ctx, n, "", []byte("sec")); err != nil {
			t.Fatalf("CreateWebhookSource(%s): %v", n, err)
		}
	}

	list, err := s.ListWebhookSources(ctx)
	if err != nil {
		t.Fatalf("ListWebhookSources: %v", err)
	}
	if len(list) != len(names) {
		t.Fatalf("got %d rows, want %d", len(list), len(names))
	}

	want := []string{"alpha", "bravo", "mike", "zulu"}
	for i, w := range want {
		if list[i].Source != w {
			t.Errorf("list[%d].Source = %q, want %q", i, list[i].Source, w)
		}
	}
}

func TestListWebhookSources_EmptyStore(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	list, err := s.ListWebhookSources(ctx)
	if err != nil {
		t.Fatalf("ListWebhookSources: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("got %d rows, want 0", len(list))
	}
}

func TestDeleteWebhookSource_HappyPath(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.CreateWebhookSource(ctx, "to-delete", "", []byte("sec")); err != nil {
		t.Fatalf("CreateWebhookSource: %v", err)
	}

	if err := s.DeleteWebhookSource(ctx, "to-delete"); err != nil {
		t.Fatalf("DeleteWebhookSource: %v", err)
	}

	_, err := s.WebhookSource(ctx, "to-delete")
	if err != ErrWebhookSourceNotFound {
		t.Errorf("expected ErrWebhookSourceNotFound after delete, got %v", err)
	}
}

func TestDeleteWebhookSource_NotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	err := s.DeleteWebhookSource(ctx, "never-existed")
	if err != ErrWebhookSourceNotFound {
		t.Errorf("err = %v, want ErrWebhookSourceNotFound", err)
	}
}

func TestDeleteWebhookSource_SecondDelete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.CreateWebhookSource(ctx, "src", "", []byte("sec")); err != nil {
		t.Fatalf("CreateWebhookSource: %v", err)
	}
	if err := s.DeleteWebhookSource(ctx, "src"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	err := s.DeleteWebhookSource(ctx, "src")
	if err != ErrWebhookSourceNotFound {
		t.Errorf("second delete: err = %v, want ErrWebhookSourceNotFound", err)
	}
}

func TestMarkWebhookSourceUsed_PopulatesLastUsedAt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.CreateWebhookSource(ctx, "src", "", []byte("sec")); err != nil {
		t.Fatalf("CreateWebhookSource: %v", err)
	}

	before, err := s.WebhookSource(ctx, "src")
	if err != nil {
		t.Fatalf("WebhookSource (before): %v", err)
	}
	if before.LastUsedAt.Valid {
		t.Fatal("LastUsedAt should be NULL before MarkWebhookSourceUsed")
	}

	if err := s.MarkWebhookSourceUsed(ctx, "src"); err != nil {
		t.Fatalf("MarkWebhookSourceUsed: %v", err)
	}

	after, err := s.WebhookSource(ctx, "src")
	if err != nil {
		t.Fatalf("WebhookSource (after): %v", err)
	}
	if !after.LastUsedAt.Valid {
		t.Error("LastUsedAt should be non-NULL after MarkWebhookSourceUsed")
	}
	if after.LastUsedAt.Int64 == 0 {
		t.Error("LastUsedAt should be a non-zero timestamp")
	}
}

func TestMarkWebhookSourceUsed_UnknownSource_NoError(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	err := s.MarkWebhookSourceUsed(ctx, "ghost")
	if err != nil {
		t.Errorf("MarkWebhookSourceUsed on unknown source: %v (want nil — best effort)", err)
	}
}

func TestRecordWebhookDelivery_RoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for _, d := range []*WebhookDelivery{
		{Source: "github-prod", Status: "accepted", StackName: "abbas", Revision: 1789849639, RepoURL: "https://github.com/nextrum-sy/abbas", File: "deploy/deploy.yaml"},
		{Source: "github-prod", Status: "bad_request", StackName: "abbas", Error: "deploy provenance required"},
		{Source: "github-cms", Status: "unauthorized"},
	} {
		if err := s.RecordWebhookDelivery(ctx, d); err != nil {
			t.Fatalf("RecordWebhookDelivery: %v", err)
		}
	}

	got, err := s.ListWebhookDeliveries(ctx, "github-prod", 0)
	if err != nil {
		t.Fatalf("ListWebhookDeliveries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].StackName != "abbas" || got[0].Status != "bad_request" || got[0].Error == "" {
		t.Errorf("newest-first ordering wrong: %+v", got[0])
	}
	if got[1].Revision != 1789849639 || got[1].File != "deploy/deploy.yaml" {
		t.Errorf("second delivery provenance wrong: %+v", got[1])
	}

	limited, err := s.ListWebhookDeliveries(ctx, "github-prod", 1)
	if err != nil {
		t.Fatalf("ListWebhookDeliveries limited: %v", err)
	}
	if len(limited) != 1 || limited[0].Status != "bad_request" {
		t.Errorf("limit=1 got %d rows (want 1, newest first)", len(limited))
	}

	other, err := s.ListWebhookDeliveries(ctx, "github-cms", 0)
	if err != nil {
		t.Fatalf("ListWebhookDeliveries github-cms: %v", err)
	}
	if len(other) != 1 || other[0].Status != "unauthorized" {
		t.Errorf("source-scoped query wrong: %+v", other)
	}
}

// TestRecordWebhookDelivery_RetriesRoundTrip covers migration 0019 through
// the store: a delivery recorded with Retries=2 reads back with retries=2,
// while a delivery that never retried (the field left unset by older call
// sites) reads back as 0.
func TestRecordWebhookDelivery_RetriesRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	exhausted := &WebhookDelivery{
		Source:  "github-prod",
		Status:  "server_error",
		Retries: 2,
		Error:   "docker stack deploy failed: final attempt",
	}
	firstTry := &WebhookDelivery{
		Source:    "github-prod",
		Status:    "accepted",
		StackName: "abbas",
		Revision:  7,
		// Retries deliberately unset → 0.
	}

	if err := s.RecordWebhookDelivery(ctx, exhausted); err != nil {
		t.Fatalf("RecordWebhookDelivery (retried): %v", err)
	}
	if err := s.RecordWebhookDelivery(ctx, firstTry); err != nil {
		t.Fatalf("RecordWebhookDelivery (first try): %v", err)
	}

	got, err := s.ListWebhookDeliveries(ctx, "github-prod", 0)
	if err != nil {
		t.Fatalf("ListWebhookDeliveries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// Newest first: the first-try row was inserted last (same second, higher id).
	if got[0].Status != "accepted" || got[0].Retries != 0 {
		t.Errorf("first-try delivery = %+v, want status accepted with retries 0", got[0])
	}
	if got[1].Status != "server_error" || got[1].Retries != 2 {
		t.Errorf("retried delivery = %+v, want status server_error with retries 2", got[1])
	}
	if got[1].Error != "docker stack deploy failed: final attempt" {
		t.Errorf("retried delivery error = %q, want the final error message", got[1].Error)
	}
}
