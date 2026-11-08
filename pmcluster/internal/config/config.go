// Package config loads the pmcluster runtime config from YAML and
// PMCLUSTER_* environment variables, in that priority order.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/logging"
)

// Config is the resolved runtime config; constructed via Load.
type Config struct {
	ListenAddr string `mapstructure:"listen_addr"`
	DataDir    string `mapstructure:"data_dir"`
	LogLevel   string `mapstructure:"log_level"`

	// OTLPEndpoint is the base URL of the OTel Collector that pmcluster's own
	// daemon ships traces/metrics/logs to. It defaults to OTLPAutoEndpoint,
	// which resolves THIS node's swarm address at startup (see
	// SelfTelemetryEndpoint) and never names a fixed host. Empty disables
	// self-telemetry.
	OTLPEndpoint string `mapstructure:"otlp_endpoint"`
}

// The derived path helpers resolve the well-known files and directories under
// DataDir: the SQLite database, the encryption key, the operator config file,
// the operator config directory, and the logs directory.
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
		// 0.0.0.0 so the edge console container can reach the daemon via
		// host.docker.internal:9090 (the docker bridge gateway). Binding
		// loopback (127.0.0.1) breaks the console with 'context deadline
		// exceeded' on freshly initialised clusters.
		ListenAddr:   "0.0.0.0:9090",
		DataDir:      filepath.Join(home, ".pmcluster"),
		LogLevel:     "info",
		OTLPEndpoint: OTLPAutoEndpoint,
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

	// viper's AutomaticEnv treats an EMPTY env value as "not set", so
	// PMCLUSTER_OTLP_ENDPOINT= would quietly fall back to the "auto" default
	// while every other spelling of empty means disabled. Collapse that
	// difference: an empty otlp_endpoint turns self-telemetry off, whatever
	// source it came from.
	if val, ok := os.LookupEnv("PMCLUSTER_OTLP_ENDPOINT"); ok && strings.TrimSpace(val) == "" {
		cfg.OTLPEndpoint = ""
	}

	cfg.normalizeLogLevel()
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
	return nil
}

// normalizeLogLevel settles a log level from any source onto the accepted
// vocabulary. An unknown value is NOT fatal: a typo in a systemd unit or a
// config file must leave the daemon running at info, with the shared logging
// helper printing the one warning that names the rejected value (see
// internal/logging.ConfigureFromEnv). Failing here would make the warning
// unreachable — the process would exit before writing it.
func (c *Config) normalizeLogLevel() {
	c.LogLevel = logging.LevelOrDefault(c.LogLevel, logging.DefaultLevel)
}
