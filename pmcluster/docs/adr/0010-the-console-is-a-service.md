# ADR 0010 — the console is a service like any other: it ships its own logs and spans

**Status:** accepted (2026-11-17)

## Context

Every other component on this platform can be investigated from the observability backend. The
console could not. Two separate reasons, and neither was a bug in anyone's code:

- **Its container logs were never scraped.** The collector's `receiver_creator` deliberately skips
  the `edge` stack (`labels["com.docker.stack.namespace"] != "edge"`), and the logs pipeline's
  `filter/skip_filelog` drops unattributable records. That exclusion was written when the edge's
  access-log volume was noise with no query value — which it was. The consequence was never stated
  in one place: the one component an operator stares at all day had no logs anywhere.
- **Nothing produced its spans.** Traefik was the platform's only span producer before it was turned
  down (`--tracing.otlp=false`), and the daemon's own instrumentation covers the daemon's HTTP
  server, not the console's. So `pmcluster-traces` had no console traces to show, and a slow page
  could only be argued about, not measured.

## Decision

**The console emits OTLP itself.** It does not depend on being scraped, and it does not need the
collector's container rules to change.

- `cmd/edge` calls `telemetry.Init` with `service.name = pmcluster-edge`, so its logs, spans and any
  future metrics arrive through the same collector as the daemon's.
- **Endpoint:** `OTEL_EXPORTER_OTLP_ENDPOINT=http://host.docker.internal:4318`, set by the edge
  stack. The collector publishes OTLP/HTTP on the **leader's host network** (`mode: host`) and the
  edge is placed on the leader, so that hostname is the path from inside the container. No
  `monitoring-net` membership is needed and none was added. Unset means **no exporter**: a standalone
  console on a laptop must not point at an address nobody answers on — that is the shape of the
  2026-11-08 journal storm.
- **Node identity** comes from `PMCLUSTER_NODE`, which the stack renders from the leader's hostname.
  Inside a container `os.Hostname()` is the container id, so without it the console's records would be
  the only ones in the platform with no machine attached.
- **Logs** ride a structured sink: `logging.ConfigureFromEnvWithSink` keeps the human console writer
  AND writes every event as zerolog JSON into `logger.OTelWriter()`, which decodes it into an OTel log
  record with severity, message and fields. The sink must receive **JSON**, not the console writer's
  coloured text — a bridge handed coloured text decodes nothing and drops every record silently, which
  is why the tee is a tested property rather than an assumption.
- **Spans** come from `otelhttp`: an inbound handler span named `pmcluster-edge` around the whole
  proxy-plus-console handler, and a client span per outbound call. The outbound side instruments
  `http.DefaultTransport`, because the reverse proxy builds a `ReverseProxy` with no transport of its
  own and the console's API client uses the default one — one write covers both instead of threading
  a client through two constructors.

## Consequences

- The console's logs now carry a real level (the pipeline parses zerolog's JSON at ingest), a
  `service.name`, a node, and a trace id when the line is written inside a request — so a log row and
  the span it belongs to can be joined, which was impossible when the edge was invisible.
- **Volume is a real cost and it is capped at the producer.** The `log_level` cluster setting already
  reaches the edge as `PMCLUSTER_LOG_LEVEL`; a per-request access log at info is the thing to watch,
  and turning it down is a producer decision, never a collector-side filter.
- The `edge` exclusion in `receiver_creator` **stays**. It is right: scraping that container would
  duplicate what OTLP now delivers, at a worse level of structure.
- A console that cannot reach the collector still serves its pages. Export failures are rate-limited
  by the same `otel.SetErrorHandler` supervisor the daemon uses — one line per distinct failure, then
  a summary — so a down collector cannot flood the node's journal.
- The same rule now applies to any future platform component: **if the collector does not scrape it,
  the component must ship OTLP itself**, and saying "it is on the leader, the scrape will find it" is
  how a service ends up invisible for months.
