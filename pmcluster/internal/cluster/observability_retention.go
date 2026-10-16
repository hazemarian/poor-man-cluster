package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ParseableRetentionBaseURL is where the daemon reaches Parseable to converge
// per-stream retention. Parseable publishes 8000 on the routing mesh for
// exactly this reason: the daemon is a HOST process (systemd), not a member of
// a swarm network, so it cannot resolve observability_parseable — it reaches
// the mesh address instead. Same idiom as SeaweedFS publishing 8333.
const ParseableRetentionBaseURL = "http://127.0.0.1:8000"

// Parseable's retention contract, established by probing the running instance
// rather than from documentation:
//
//	GET  /api/v1/logstream/{stream}/retention  -> [ {duration,action,description} ]
//	PUT  /api/v1/logstream/{stream}/retention  <- [ {duration,action,description} ]
//
// The body MUST be a JSON sequence of task objects and `action` is mandatory;
// a bare object is rejected with "invalid type: map, expected a sequence" and an
// object without `action` with "missing field `action`". Duration carries a unit
// suffix ("30d", "24h").
const (
	retentionStreamLogs    = "pmcluster-logs"
	retentionStreamMetrics = "pmcluster-metrics"
	retentionStreamTraces  = "pmcluster-traces"
)

// retentionTask mirrors one element of Parseable's retention sequence.
type retentionTask struct {
	Duration    string `json:"duration"`
	Action      string `json:"action"`
	Description string `json:"description"`
}

// ConvergeObservabilityRetention applies the cluster's retention settings to
// Parseable's streams. OpenObserve took retention as ZO_*_RETENTION_DAYS env
// vars; Parseable takes none from the environment, so the settings only mean
// anything if the daemon pushes them — without this, streams grow unbounded and
// the working set with them.
//
// A non-positive day count means "not configured" and that stream is skipped,
// leaving whatever retention is already in place alone rather than silently
// removing it.
func ConvergeObservabilityRetention(ctx context.Context, baseURL, user, password string, logs, metrics, traces int) error {
	streams := []struct {
		name string
		days int
	}{
		{retentionStreamLogs, logs},
		{retentionStreamMetrics, metrics},
		{retentionStreamTraces, traces},
	}

	client := &http.Client{Timeout: 15 * time.Second}
	for _, s := range streams {
		if s.days <= 0 {
			continue
		}
		if err := setStreamRetention(ctx, client, baseURL, user, password, s.name, s.days); err != nil {
			return fmt.Errorf("stream %s: %w", s.name, err)
		}
	}
	return nil
}

// setStreamRetention PUTs one stream's retention. Returned separately so the
// caller can report which stream failed.
func setStreamRetention(ctx context.Context, client *http.Client, baseURL, user, password, stream string, days int) error {
	body, err := json.Marshal([]retentionTask{{
		Duration:    fmt.Sprintf("%dd", days),
		Action:      "delete",
		Description: fmt.Sprintf("pmcluster %s retention", stream),
	}})
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/api/v1/logstream/%s/retention", baseURL, stream)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(user, password)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}
