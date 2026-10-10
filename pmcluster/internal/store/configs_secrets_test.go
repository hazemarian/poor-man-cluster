package store

import (
	"context"
	"errors"
	"testing"
)

func TestConfigsCRUD(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	t.Run("create + get roundtrip", func(t *testing.T) {
		id, err := s.CreateConfig(ctx, "cluster", "", "traefik_dynamic", "template",
			"http:\n  middlewares:\n    cors-default:\n      cors:\n        allowOrigins:\n          - \"https://example.com\"\n", "v0.2.30")
		if err != nil {
			t.Fatalf("CreateConfig: %v", err)
		}
		if id <= 0 {
			t.Errorf("CreateConfig id = %d, want > 0", id)
		}

		c, err := s.Config(ctx, "traefik_dynamic")
		if err != nil {
			t.Fatalf("Config: %v", err)
		}
		if c.Name != "traefik_dynamic" || c.Scope != "cluster" || c.Kind != "template" {
			t.Errorf("Config = %+v", c)
		}
		if c.Hash != ConfigHash(c.Content) {
			t.Errorf("hash = %q, want %q", c.Hash, ConfigHash(c.Content))
		}
		if c.Version != "v0.2.30" {
			t.Errorf("version = %q, want v0.2.30", c.Version)
		}
	})

	t.Run("duplicate name returns ErrConfigExists", func(t *testing.T) {
		_, err := s.CreateConfig(ctx, "cluster", "", "traefik_dynamic", "template", "x", "v0.2.30")
		if !errors.Is(err, ErrConfigExists) {
			t.Errorf("CreateConfig dup err = %v, want ErrConfigExists", err)
		}
	})

	t.Run("get missing returns ErrConfigNotFound", func(t *testing.T) {
		_, err := s.Config(ctx, "nope")
		if !errors.Is(err, ErrConfigNotFound) {
			t.Errorf("Config missing err = %v, want ErrConfigNotFound", err)
		}
	})

	t.Run("list orders by scope then name", func(t *testing.T) {
		_, _ = s.CreateConfig(ctx, "service", "", "nginx_conf", "file", "server {}", "v0.2.30")
		_, _ = s.CreateConfig(ctx, "cluster", "", "otel_collector", "template", "receivers: {}", "v0.2.30")

		cfgs, err := s.ListConfigs(ctx, "", "")
		if err != nil {
			t.Fatalf("ListConfigs: %v", err)
		}
		if len(cfgs) != 3 {
			t.Fatalf("ListConfigs = %d rows, want 3", len(cfgs))
		}

		want := []string{"otel_collector", "traefik_dynamic", "nginx_conf"}
		for i, w := range want {
			if cfgs[i].Name != w {
				t.Errorf("cfgs[%d].Name = %q, want %q", i, cfgs[i].Name, w)
			}
		}
	})

	t.Run("stack filter includes unattached service rows", func(t *testing.T) {
		// A service-scope config created without --stack (CLI default) is
		// resolved by name from any stack's DSL — it must show up under a
		// stack-filtered listing, not just the "all" listing.
		if _, err := s.CreateConfig(ctx, "service", "", "shared_api_key", "env", "KEY=v", "v0.2.30"); err != nil {
			t.Fatalf("CreateConfig unattached: %v", err)
		}
		cfgs, err := s.ListConfigs(ctx, "service", "demo")
		if err != nil {
			t.Fatalf("ListConfigs(service, demo): %v", err)
		}
		found := false
		for _, c := range cfgs {
			if c.Name == "shared_api_key" {
				found = true
				if c.Stack != "" {
					t.Errorf("shared_api_key stack = %q, want ''", c.Stack)
				}
			}
		}
		if !found {
			t.Errorf("ListConfigs(service, demo) missing unattached service row shared_api_key")
		}
		// Cluster-scope rows still excluded under a service filter.
		for _, c := range cfgs {
			if c.Scope != "service" {
				t.Errorf("ListConfigs(service, demo) leaked %s/%s", c.Scope, c.Name)
			}
		}
	})

	t.Run("secrets stack filter includes unattached service rows", func(t *testing.T) {
		if _, err := s.CreateSecret(ctx, "service", "", "shared_db_pass", []byte("cipher"), "h1"); err != nil {
			t.Fatalf("CreateSecret unattached: %v", err)
		}
		secs, err := s.ListSecrets(ctx, "service", "demo")
		if err != nil {
			t.Fatalf("ListSecrets(service, demo): %v", err)
		}
		found := false
		for _, sec := range secs {
			if sec.Name == "shared_db_pass" {
				found = true
			}
			if sec.Scope != "service" {
				t.Errorf("ListSecrets(service, demo) leaked %s/%s", sec.Scope, sec.Name)
			}
		}
		if !found {
			t.Errorf("ListSecrets(service, demo) missing unattached service row shared_db_pass")
		}
	})
}

func TestConfigUpdateVersionsRollback(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, err := s.CreateConfig(ctx, "service", "", "app_conf", "env", "LOG_LEVEL=info", "v0.2.30")
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}

	t.Run("update changes content + hash, preserves history", func(t *testing.T) {
		h, err := s.UpdateConfig(ctx, "app_conf", "LOG_LEVEL=debug", "v0.2.30")
		if err != nil {
			t.Fatalf("UpdateConfig: %v", err)
		}
		if h != ConfigHash("LOG_LEVEL=debug") {
			t.Errorf("new hash = %q", h)
		}

		c, _ := s.Config(ctx, "app_conf")
		if c.Content != "LOG_LEVEL=debug" {
			t.Errorf("content = %q, want LOG_LEVEL=debug", c.Content)
		}

		vers, err := s.ListConfigVersions(ctx, "app_conf")
		if err != nil {
			t.Fatalf("ListConfigVersions: %v", err)
		}
		if len(vers) != 1 {
			t.Fatalf("versions = %d, want 1", len(vers))
		}
		if vers[0].Content != "LOG_LEVEL=info" {
			t.Errorf("version[0].Content = %q, want original LOG_LEVEL=info", vers[0].Content)
		}
	})

	t.Run("no-op update does not create a version", func(t *testing.T) {
		_, err := s.UpdateConfig(ctx, "app_conf", "LOG_LEVEL=debug", "v0.2.30")
		if err != nil {
			t.Fatalf("UpdateConfig (no-op): %v", err)
		}
		vers, _ := s.ListConfigVersions(ctx, "app_conf")
		if len(vers) != 1 {
			t.Errorf("versions after no-op = %d, want 1", len(vers))
		}
	})

	t.Run("rollback restores previous version", func(t *testing.T) {
		vers, _ := s.ListConfigVersions(ctx, "app_conf")
		if len(vers) != 1 {
			t.Fatalf("need 1 version for rollback, have %d", len(vers))
		}
		h, err := s.RollbackConfig(ctx, "app_conf", vers[0].ID)
		if err != nil {
			t.Fatalf("RollbackConfig: %v", err)
		}
		if h != ConfigHash("LOG_LEVEL=info") {
			t.Errorf("rollback hash = %q, want original", h)
		}
		c, _ := s.Config(ctx, "app_conf")
		if c.Content != "LOG_LEVEL=info" {
			t.Errorf("content after rollback = %q, want LOG_LEVEL=info", c.Content)
		}
	})

	t.Run("rollback with foreign version id fails", func(t *testing.T) {
		_, _ = s.CreateConfig(ctx, "service", "", "other_conf", "file", "a", "v0.2.30")

		appVers, _ := s.ListConfigVersions(ctx, "app_conf")
		if len(appVers) > 0 {
			_, err := s.RollbackConfig(ctx, "other_conf", appVers[0].ID)
			if !errors.Is(err, ErrConfigVersionNotFound) {
				t.Errorf("cross-config rollback err = %v, want ErrConfigVersionNotFound", err)
			}
		}
	})

	t.Run("update missing config returns ErrConfigNotFound", func(t *testing.T) {
		_, err := s.UpdateConfig(ctx, "ghost", "x", "v0.2.30")
		if !errors.Is(err, ErrConfigNotFound) {
			t.Errorf("UpdateConfig ghost err = %v, want ErrConfigNotFound", err)
		}
	})
}

func TestConfigDelete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, err := s.CreateConfig(ctx, "service", "", "gone", "file", "x", "v0.2.30")
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	_, _ = s.UpdateConfig(ctx, "gone", "y", "v0.2.30")

	if err := s.DeleteConfig(ctx, "gone"); err != nil {
		t.Fatalf("DeleteConfig: %v", err)
	}
	if _, err := s.Config(ctx, "gone"); !errors.Is(err, ErrConfigNotFound) {
		t.Errorf("Config after delete = %v, want ErrConfigNotFound", err)
	}

	vers, err := s.ListConfigVersions(ctx, "gone")
	if !errors.Is(err, ErrConfigNotFound) {
		t.Errorf("ListConfigVersions after delete = %v, want ErrConfigNotFound", err)
	}
	if vers != nil {
		t.Errorf("expected nil versions, got %d", len(vers))
	}

	if err := s.DeleteConfig(ctx, "gone"); !errors.Is(err, ErrConfigNotFound) {
		t.Errorf("DeleteConfig twice = %v, want ErrConfigNotFound", err)
	}
}

func TestSecretsCRUD(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	t.Run("create + get roundtrip", func(t *testing.T) {
		payload := []byte("\x01\x02\x03nonce+seal\x04")
		id, err := s.CreateSecret(ctx, "service", "", "db_password", payload, "abc123hash")
		if err != nil {
			t.Fatalf("CreateSecret: %v", err)
		}
		if id <= 0 {
			t.Errorf("CreateSecret id = %d, want > 0", id)
		}

		r, err := s.Secret(ctx, "db_password")
		if err != nil {
			t.Fatalf("Secret: %v", err)
		}
		if r.Scope != "service" || r.Hash != "abc123hash" {
			t.Errorf("Secret = %+v", r)
		}
		if string(r.Payload) != string(payload) {
			t.Errorf("payload mismatch")
		}
	})

	t.Run("duplicate name returns ErrSecretExists", func(t *testing.T) {
		_, err := s.CreateSecret(ctx, "service", "", "db_password", []byte("x"), "h")
		if !errors.Is(err, ErrSecretExists) {
			t.Errorf("CreateSecret dup err = %v, want ErrSecretExists", err)
		}
	})

	t.Run("get missing returns ErrSecretNotFound", func(t *testing.T) {
		_, err := s.Secret(ctx, "nope")
		if !errors.Is(err, ErrSecretNotFound) {
			t.Errorf("Secret missing err = %v, want ErrSecretNotFound", err)
		}
	})

	t.Run("list excludes payload", func(t *testing.T) {
		_, _ = s.CreateSecret(ctx, "cluster", "", "cert_nextrum", []byte("c"), "h1")
		_, _ = s.CreateSecret(ctx, "service", "", "api_token", []byte("t"), "h2")

		secrets, err := s.ListSecrets(ctx, "", "")
		if err != nil {
			t.Fatalf("ListSecrets: %v", err)
		}
		if len(secrets) != 3 {
			t.Fatalf("ListSecrets = %d rows, want 3", len(secrets))
		}

		want := []string{"cert_nextrum", "api_token", "db_password"}
		for i, w := range want {
			if secrets[i].Name != w {
				t.Errorf("secrets[%d].Name = %q, want %q", i, secrets[i].Name, w)
			}
			if secrets[i].Payload != nil {
				t.Errorf("secrets[%d] leaked payload", i)
			}
		}
	})

	t.Run("delete", func(t *testing.T) {
		if err := s.DeleteSecret(ctx, "api_token"); err != nil {
			t.Fatalf("DeleteSecret: %v", err)
		}
		if _, err := s.Secret(ctx, "api_token"); !errors.Is(err, ErrSecretNotFound) {
			t.Errorf("Secret after delete = %v, want ErrSecretNotFound", err)
		}
		if err := s.DeleteSecret(ctx, "api_token"); !errors.Is(err, ErrSecretNotFound) {
			t.Errorf("DeleteSecret twice = %v, want ErrSecretNotFound", err)
		}
	})

	t.Run("update bumps swarm_rev (BUG-007)", func(t *testing.T) {
		if err := s.UpdateSecret(ctx, "db_password", []byte("newseal"), "newhash"); err != nil {
			t.Fatalf("UpdateSecret: %v", err)
		}
		r, err := s.Secret(ctx, "db_password")
		if err != nil {
			t.Fatalf("Secret after update: %v", err)
		}
		if r.SwarmRev != 2 {
			t.Errorf("swarm_rev = %d, want 2 after one edit", r.SwarmRev)
		}
		if r.Hash != "newhash" || string(r.Payload) != "newseal" {
			t.Errorf("updated row = %+v, want new payload/hash", r)
		}
		// List must expose the rev too (drives the compose writer + CLI).
		rows, err := s.ListSecrets(ctx, "", "")
		if err != nil {
			t.Fatalf("ListSecrets: %v", err)
		}
		for _, row := range rows {
			if row.Name == "db_password" && row.SwarmRev != 2 {
				t.Errorf("ListSecrets row.SwarmRev = %d, want 2", row.SwarmRev)
			}
		}
		// A second edit bumps again.
		if err := s.UpdateSecret(ctx, "db_password", []byte("seal3"), "hash3"); err != nil {
			t.Fatalf("UpdateSecret 2: %v", err)
		}
		r2, _ := s.Secret(ctx, "db_password")
		if r2.SwarmRev != 3 {
			t.Errorf("swarm_rev = %d, want 3 after two edits", r2.SwarmRev)
		}
	})

	t.Run("SwarmSecretName helper (content-addressed)", func(t *testing.T) {
		cases := []struct {
			name string
			hash string
			want string
		}{
			{"app_pass", "", "app_pass"},
			{"app_pass", "abc", "app_pass"},
			{"app_pass", "abcdefgh12345678", "app_pass_abcdefgh"},
			{"db_password", "1234567890abcdef", "db_password_12345678"},
		}
		for _, c := range cases {
			if got := SwarmSecretName(c.name, c.hash); got != c.want {
				t.Errorf("SwarmSecretName(%q, %q) = %q, want %q", c.name, c.hash, got, c.want)
			}
		}
	})

	t.Run("SwarmConfigName helper (content-addressed)", func(t *testing.T) {
		cases := []struct {
			name string
			hash string
			want string
		}{
			{"app_config", "", "app_config"},
			{"app_config", "abc", "app_config"},
			{"app_config", "abcdefgh12345678", "app_config_abcdefgh"},
			{"pmcluster_otel_config", "1234567890abcdef", "pmcluster_otel_config_12345678"},
		}
		for _, c := range cases {
			if got := SwarmConfigName(c.name, c.hash); got != c.want {
				t.Errorf("SwarmConfigName(%q, %q) = %q, want %q", c.name, c.hash, got, c.want)
			}
		}
	})
}
