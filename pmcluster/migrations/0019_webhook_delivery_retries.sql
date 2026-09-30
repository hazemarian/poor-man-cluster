-- 0019_webhook_delivery_retries.sql
--
-- A webhook-triggered deploy that fails transiently (delivery status
-- server_error) is now retried up to 2 extra times, 30s apart, before the
-- receiver gives up. The delivery row records how many retries actually
-- ran, so the console/CLI can tell a first-try failure apart from one that
-- burned through the whole retry budget. Legacy rows keep retries = 0.
ALTER TABLE webhook_deliveries ADD COLUMN retries INTEGER NOT NULL DEFAULT 0;
