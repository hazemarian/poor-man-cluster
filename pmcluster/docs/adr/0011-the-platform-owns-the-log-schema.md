# 0011 — The platform owns the log schema; an app's JSON stays in the body

**Status:** accepted · 2026-11-18

## The decision

The OTel collector keeps only the attributes the **platform** sets on a log record
(`stream`, `log.file.path`, `time`, `level`) and drops everything else before the batch
reaches the store. An application's structured fields are not promoted to attributes:
they stay in the record body, byte-for-byte, and are searched as text
(`body ILIKE '%term%'`).

The list of kept keys lives in one place — the `transform/logs_attributes` processor in
`internal/cluster/embeds/otel-collector-config.yml` — and its **position in the pipeline** is
asserted by `internal/cluster/templates_test.go`, because a processor that is configured but
placed after the batch exporter is indistinguishable from one that is not configured at all.

## Why

Deriving attributes from arbitrary application JSON means the platform executes a schema it does
not control, on behalf of apps that do not know each other.

Measured, not hypothesised: one application logged `"category_id": null`. A JSON `null` has no
OTLP equivalent, so it arrived as `{"key":"category_id","value":{}}` — an attribute with an empty
value — and the store rejects **the whole batch** that carries such a record. The collector ships
every service's logs in the same batches, so for ten hours the entire platform ingested nothing
while every service kept logging normally. It was found by bisecting a 153-record rejected batch
down to a single record.

The asymmetry that decides it: an app that ships a broken attribute blinds every service, whereas
an app that ships unusual JSON has unusual text. Text is a local problem; schema is a shared one.

## What was rejected

- **Whitelist scalar keys** (`status`, `duration_ms`, `method`, `path`). Cheap, and it makes the
  common case nicer, but it re-opens the same hole the first time an app sends one of those keys as
  `null`, and the failure mode is not "that app's logs are wrong" — it is "nobody's logs arrive".
- **Have apps send OTLP themselves.** Correct for an app that wants structure, and `skip_filelog`
  already exists for it, but it cannot be the platform's answer: the balance of apps on this
  cluster log to stdout and always will.
- **Guards on the *shape* of the payload** (control characters, `\u0000`, empty strings, absent
  body). Three were added and two were deleted afterwards as dead: the store accepted every one of
  those shapes. A guard that matches nothing is worse than no guard, because it reads as covered.

## What it costs

- Searching a structured field means text search, and a `null` field is only visible as the literal
  text `null`.
- Dashboards cannot break down by an app field without the app shipping OTLP.
- The platform can no longer claim "your JSON is queryable" — it can claim "your JSON is in the
  body, unchanged, and searchable".
