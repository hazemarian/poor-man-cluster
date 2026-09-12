// Package edgeproxy implements the pmcluster-edge smart reverse proxy.
//
// It sits between Traefik (the TLS edge) and the pmcluster host daemon,
// adding the protections the daemon doesn't provide: per-real-IP throttling,
// a connection/DDoS shield, and IP blocklisting with auto-ban. It is the proxy
// half of the combined edge service; the gin+HTMX operator console is mounted
// beside it by cmd/edge.
//
// Chosen design (see plan.md): stdlib net/http/httputil.ReverseProxy + chi,
// reusing golang.org/x/time/rate (already a module dependency). The edge is
// deliberately socketless — it does not mount the Docker socket and does not
// talk to Docker directly; cluster data flows through the pmcluster API.
package edgeproxy

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every tunable for the edge service. Fields are populated from
// environment variables by FromEnv, with sane defaults for a manager node.
type Config struct {
	ListenAddr string // HTTP listen address (default ":8042")
	Upstream   string // pmcluster daemon base URL (default "http://host.docker.internal:9090")

	// Per-IP rate limits (token bucket), keyed by the real client IP. Separate
	// buckets for /api and /webhook so a noisy CI webhook can't starve the API.
	APIRate      float64
	APIBurst     int
	WebhookRate  float64
	WebhookBurst int

	// Connection/DDoS shield.
	MaxConcurrent  int           // max in-flight proxied requests (0 = unlimited)
	RequestTimeout time.Duration // per-request deadline to upstream
	MaxBodyBytes   int64         // max request body forwarded (0 = default 1 MiB)

	// IP trust + blocklisting. TrustedCIDRs are proxies we accept
	// X-Forwarded-For from (Traefik runs in an overlay). AllowedCIDRs, when
	// non-empty, whitelists client IPs; BannedCIDRs blacklists them.
	TrustedCIDRs []string
	AllowedCIDRs []string
	BannedCIDRs  []string

	// Auto-ban: an IP tripping BanThreshold failures (rate-limit 429s or
	// upstream 401/403) is blocked for BanDuration.
	BanThreshold int
	BanDuration  time.Duration

	LogLevel string
}

// Defaults mirroring the daemon's own generous limits, tuned outward for the
// public edge.
const (
	defaultAPIRate        = 200.0
	defaultAPIBurst       = 400
	defaultWebhookRate    = 40.0
	defaultWebhookBurst   = 60
	defaultMaxConcurrent  = 128
	defaultMaxBodyBytes   = 1 << 20 // 1 MiB (webhook already caps here)
	defaultRequestTimeout = 30 * time.Second
	defaultBanThreshold   = 20
	defaultBanDuration    = 10 * time.Minute
)

// envOr returns env[key] if non-empty, else def.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envFloat parses env[key] as a float64, falling back to def on absence/error.
func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// envInt parses env[key] as an int, falling back to def on absence/error.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

// envDur parses env[key] as a Go duration string, falling back to def.
func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// splitList splits a comma-separated env value into trimmed non-empty tokens.
func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// FromEnv builds a Config from the process environment with defaults.
func FromEnv() Config {
	return Config{
		ListenAddr:     envOr("LISTEN_ADDR", ":8042"),
		Upstream:       envOr("UPSTREAM", "http://host.docker.internal:9090"),
		APIRate:        envFloat("API_RATE", defaultAPIRate),
		APIBurst:       envInt("API_BURST", defaultAPIBurst),
		WebhookRate:    envFloat("WEBHOOK_RATE", defaultWebhookRate),
		WebhookBurst:   envInt("WEBHOOK_BURST", defaultWebhookBurst),
		MaxConcurrent:  envInt("MAX_CONCURRENT", defaultMaxConcurrent),
		RequestTimeout: envDur("REQUEST_TIMEOUT", defaultRequestTimeout),
		MaxBodyBytes:   int64(envInt("MAX_BODY_BYTES", defaultMaxBodyBytes)),
		TrustedCIDRs:   splitList(envOr("TRUSTED_PROXY_CIDRS", "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,127.0.0.1/8")),
		AllowedCIDRs:   splitList(envOr("ALLOWED_CIDRS", "")),
		BannedCIDRs:    splitList(envOr("BANNED_CIDRS", "")),
		BanThreshold:   envInt("BAN_THRESHOLD", defaultBanThreshold),
		BanDuration:    envDur("BAN_DURATION", defaultBanDuration),
		LogLevel:       envOr("LOG_LEVEL", "info"),
	}
}
