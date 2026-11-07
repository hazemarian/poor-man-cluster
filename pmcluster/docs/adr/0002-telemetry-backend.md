# ADR 0002 — Telemetry backend: owned API over parquet, not an owned engine

Status: proposed · Date: 2026-11-07

## Context

pmcluster ships an observability stack (`internal/cluster/embeds/observability-stack.yml`):
an OTel collector as the single ingestion boundary, and Parseable as the store. The
console reads it through `internal/observability` (`Source`, `LogSource`,
`StatsSource`) via the adapter in `internal/observability/parseable`.

The access layer is ours and it works: bounded windows, row caps, a redactor,
`StatusUnavailable` instead of raw errors, and every query shape written by us
rather than by a caller. The store is the part that keeps failing, and the failures
are structural rather than configuration:

| Symptom | Evidence (measured on the production swarm, 2026-11-07) |
|---|---|
| Query cost scales with the window — no rollups | log page 1 h **0.28 s**, 24 h **2.88 s**; metrics aggregate 1 h **0.31 s**, 24 h **3.27 s** |
| A SQL dialect that rejects ordinary SQL | `GROUP BY <alias>` answers **400** ("Column in SELECT must be in GROUP BY or an aggregate function"). This silently disabled the log-volume, node-CPU/memory and trace panels for months; the console rendered them as "telemetry source unavailable", which reads as a missing data source |
| Stream type fixed at creation | a stream created by an ingest with the wrong log-source can never accept the right one; only delete-and-recreate fixes it |
| Bulk operations exceed the container | `DELETE /api/v1/logstream/<stream>` on a large stream took the container from ~300 MB to **747.5 / 768 MB** and stopped answering health checks; recovery required `docker service update --force` |
| Severity inferred from text | records are labelled by the *stream they were read from* (stderr ⇒ ERROR), so a zerolog `INF` line written to stderr is counted as an error until a regex recognises `INF` in an ANSI-escaped body |

OpenObserve was the alternative in production before this: **stable, but heavier**
than these nodes carry comfortably — 4-m has 1.8 GB total. It is also AGPL-3.0, and
the platform may be sold as a service, so its network-source obligations are a
business decision rather than a technical one.

The constraint that shapes the decision: the console and the MCP surface need a
**stable, bounded query contract**, not a full observability suite. Alerts,
dashboards and saved views are wanted from the backend whoever provides them; the
part worth owning is the API the console and the agents speak.

## Decision

Keep the OTel collector as the ingestion boundary. Replace the store behind the
existing `internal/observability` interfaces with a **single Go service using
DuckDB over parquet files**, exposing a bounded HTTP API that the console and the
MCP tools call.

The platform must not depend on a specific backend's query dialect: every statement
is written by `internal/observability` and must run unchanged against any backend
that implements the interfaces.

### Shape

```
OTel collector ──file exporter (JSON, rotating)──▶ <volume>/telemetry/inbox
                                                          │
                        ingester (Go + DuckDB, in the observability stack)
                        every minute: JSON → parquet, hourly file, atomic rename
                                                          │
                          /telemetry/logs/dt=YYYY-MM-DD/h=HH/*.parquet
                          /telemetry/metrics/...
                          /telemetry/traces/...
                                                          │
                     query API (bounded templates) ──▶ console · MCP tools
```

Hive-partitioned directories are the performance mechanism: a query that names
`dt`/`h` prunes files before reading a byte, and column projection keeps the scan
to the columns asked for. This is what removes the "cost scales with the window"
problem without inventing rollups.

### Schema (one table per signal)

Flattened OTel fields, with the long tail kept as a JSON column so an unmodelled
attribute never forces a migration:

| column | type | note |
|---|---|---|
| `ts` | TIMESTAMP | event time, the partition source |
| `service_name` | VARCHAR | from `service.name` |
| `severity_text` | VARCHAR | taken from the record, never inferred from the stream |
| `body` | VARCHAR | the message as emitted |
| `trace_id`, `span_id` | VARCHAR | empty for logs |
| `attributes` | JSON | everything not promoted to a column |

### API contract

Four shapes, matching what `internal/observability` already defines
(`LogQuery`, `TraceQuery`, `MetricQuery`, `Window`, `Result`): bounded by window,
row limit and a fixed set of query templates. **No raw SQL crosses the API** — a
caller (including an agent) names a query, not a statement.

### Retention

Delete partitions by age, plus a size cap, because the volume lives on
`/var/stack/data` (94 % free of 3.1 GiB on 4-m at the time of writing) and a burst
must not be able to fill it.

## Consequences

**What improves**

- Query cost follows the data a query names, not the window it is given.
- The dialect is DuckDB SQL — standard enough that `GROUP BY <alias>` class does
  not exist as a failure mode.
- Retention is `rm` of a directory, not an API call that can exhaust memory.
- Severity comes from the record. The regex-inference machinery in
  `internal/observability/parseable/explore.go` (`infoHintSQL`, `warnHintSQL`,
  `levelSQL`) can be deleted outright.
- Logs, metrics and traces share one store and one query path.

**What gets worse**

- **cgo.** `go-duckdb` links a C++ engine, so this cannot live in `pmcluster`'s
  multi-platform release (four tarballs, static cross-compilation,
  `CGO_ENABLED=0`). It ships as a linux/amd64 container in the observability
  stack; the platform binary stays pure Go.
- **No inverted index.** Text search is a scan with predicate pushdown. Fine at
  the measured volume (≈160 k log records/hour); it stops being fine for
  unbounded "find this string across 30 days".
- **No alerts, dashboards or saved views.** They are the reason OpenObserve was
  attractive. Owning the store means either accepting their absence, adding
  `vmalert`-shaped rules, or keeping a backend that provides them.
- **Single writer.** One ingester, serialised writes; queries are concurrent.
  Anything needing concurrent writers is out of scope by construction.
- **A new component to operate**: compaction lag, a stuck ingester and a corrupt
  parquet file are three failure modes that do not exist today.

## Verification before committing to it

The cheap experiment decides it, and it uses data already on disk: DuckDB reads
parquet directly, so point it at the **existing Parseable parquet** on the node
that runs Parseable and time the three queries that hurt — log volume by level, the
node CPU/memory aggregate, and a 24 h log page. If the numbers do not beat the
figures in Context, the design is wrong and nothing has been built.

## Handoff checklist

- [ ] Benchmark step above, on real files, with the result recorded here.
- [ ] Decide what happens to alerts/dashboards: dropped, `vmalert`, or a backend
      kept alongside.
- [ ] Ingestion path chosen and written down: collector `file` exporter → JSON →
      parquet (preferred, keeps the collector as the only ingestion boundary).
- [ ] New adapter package under `internal/observability/` implementing
      `LogSource`/`StatsSource`; no backend dialect outside it.
- [ ] Rotation, retention GC and the volume size cap in the same service.
- [ ] `internal/docscheck` passes; this record moves to **accepted** only when the
      console's observability pages run against it in production.
