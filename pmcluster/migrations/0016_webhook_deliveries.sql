-- webhook_deliveries: per-delivery history of webhook-triggered deploys.
-- Records every request outcome against a webhook source (source name,
-- outcome status, deploy provenance when available, error message) so the
-- console can show delivery history instead of only last_used_at.
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    source TEXT NOT NULL,
    status TEXT NOT NULL,           -- accepted | unauthorized | bad_request | server_error
    stack_name TEXT NOT NULL DEFAULT '',
    revision INTEGER NOT NULL DEFAULT 0,
    repo_url TEXT NOT NULL DEFAULT '',
    file TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_source_created
    ON webhook_deliveries(source, created_at DESC);