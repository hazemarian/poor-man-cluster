// Package config loads the pmcluster runtime config from YAML and
// PMCLUSTER_* environment variables, in that priority order.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

// Config is the resolved runtime config; constructed via Load.
type Config struct {
	ListenAddr string `mapstructure:"listen_addr"`
	DataDir    string `mapstructure:"data_dir"`
	LogLevel   string `mapstructure:"log_level"`

	// OTLPEndpoint is the base URL of the OTel Collector that
	// pmcluster's own daemon ships traces/metrics/logs to. The
	// observability stack publishes the OTLP/HTTP receiver on
	// 127.0.0.1:4318 on every manager node. Empty disables self-telemetry.
	OTLPEndpoint string `mapstructure:"otlp_endpoint"`
}

func (c *Config) DBPath() string            { return filepath.Join(c.DataDir, "data.db") }
func (c *Config) EncryptionKeyPath() string { return filepath.Join(c.DataDir, ".encryption_key") }
func (c *Config) ConfigPath() string        { return filepath.Join(c.DataDir, "config.yaml") }
func (c *Config) ConfigDir() string         { return filepath.Join(c.DataDir, "config") }
func (c *Config) LogsDir() string           { return filepath.Join(c.DataDir, "logs") }

func defaultConfig() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}
	return &Config{
		ListenAddr:   "127.0.0.1:9090",
		DataDir:      filepath.Join(home, ".pmcluster"),
		LogLevel:     "info",
		OTLPEndpoint: "http://127.0.0.1:4318",
	}, nil
}

// Load resolves configuration: defaults → optional YAML file → env.
// Later sources override earlier ones. A missing file is fine; a
// malformed file is an error.
func Load(configPath string) (*Config, error) {
	cfg, err := defaultConfig()
	if err != nil {
		return nil, err
	}

	v := viper.New()
	v.SetEnvPrefix("PMCLUSTER")
	v.AutomaticEnv()

	v.SetDefault("listen_addr", cfg.ListenAddr)
	v.SetDefault("data_dir", cfg.DataDir)
	v.SetDefault("log_level", cfg.LogLevel)
	v.SetDefault("otlp_endpoint", cfg.OTLPEndpoint)

	resolved := configPath
	if resolved == "" {
		resolved = cfg.ConfigPath()
	}
	v.SetConfigFile(resolved)
	v.SetConfigType("yaml")

	if err := v.ReadInConfig(); err != nil {
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			return nil, fmt.Errorf("read config %s: %w", resolved, err)
		}
		if !os.IsNotExist(pathErr) {
			return nil, fmt.Errorf("read config %s: %w", resolved, err)
		}
	}

	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.ListenAddr == "" {
		return errors.New("listen_addr is required")
	}
	if c.DataDir == "" {
		return errors.New("data_dir is required")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be one of debug|info|warn|error, got %q", c.LogLevel)
	}
	return nil
}
