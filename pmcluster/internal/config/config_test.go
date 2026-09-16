package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_Defaults exercises the no-config-file, no-env path. Sub-agent
// (Phase 1.7) should expand with: env-var overrides, malformed YAML,
// invalid log_level, --config flag pointing at non-default path.
func TestLoad_Defaults(t *testing.T) {

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:9090" {
		t.Errorf("ListenAddr default = %q, want 127.0.0.1:9090", cfg.ListenAddr)
	}
	wantData := filepath.Join(tmp, ".pmcluster")
	if cfg.DataDir != wantData {
		t.Errorf("DataDir default = %q, want %q", cfg.DataDir, wantData)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel default = %q, want info", cfg.LogLevel)
	}
	if cfg.DBPath() != filepath.Join(wantData, "data.db") {
		t.Errorf("DBPath = %q", cfg.DBPath())
	}
}

func TestLoad_EnvOverride(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PMCLUSTER_LISTEN_ADDR", "0.0.0.0:1234")
	t.Setenv("PMCLUSTER_LOG_LEVEL", "debug")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != "0.0.0.0:1234" {
		t.Errorf("env override of listen_addr did not apply: got %q", cfg.ListenAddr)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("env override of log_level did not apply: got %q", cfg.LogLevel)
	}
}

func TestLoad_RejectsBadLogLevel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PMCLUSTER_LOG_LEVEL", "verbose")

	if _, err := Load(""); err == nil {
		t.Fatal("expected error for invalid log_level, got nil")
	}
}

func TestLoad_ReadsYAMLFile(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "custom.yaml")
	if err := os.WriteFile(cfgPath, []byte("listen_addr: 127.0.0.1:7777\nlog_level: warn\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("HOME", tmp)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:7777" {
		t.Errorf("YAML override of listen_addr did not apply: got %q", cfg.ListenAddr)
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("YAML override of log_level did not apply: got %q", cfg.LogLevel)
	}
}
