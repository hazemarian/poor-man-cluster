# Nextrum build status

## Current deployment — 2026-10-19

The public console's main Observability page now federates all four tabs across
the three hosts (`nextrum-observability-20261019-4` public edge, plus corrected standalone
collector normalization on all three hosts). The old
Fleet URL is an alias. Per-node trace lists/details are available; joining
cross-backend spans into one distributed waterfall remains outstanding.

The combined three-server fleet dashboard is deployed as
`nextrum-fleet-20261019-2`; see [operations](../../deploy/fleet/README.md) and
[verification](verification/fleet-2026-10-19/README.md). Native charts,
traces, log polling, multi-provider adapters and template compaction are now
implemented; older backlog entries below predate those changes. Fleet source
changes are preserved in this checkout on
`nextrum/three-node-observability`; deployment and verification material is
under `deploy/` and `docs/nextrum/`.

Remaining: application-specific instrumentation/routing, federated trace
waterfalls/full log browsing, durable AI result history, severity normalization
at ingestion, telemetry history backups and external notifications. Fleet
does not merge AI evidence or change existing application monitoring pipelines.

## Historical status

## Native implementation

Active code is the independent nested pmcluster checkout on
`nextrum/observability-foundation`. Local changes are uncommitted. The root
repository ignores that checkout; the separate Python ObsAI prototype is parked.

Implemented and deployed in standalone monitoring mode (Swarm-specific features remain local):

- Native Observability sidebar page and service links, existing gin/HTMX styling,
  English/Arabic dictionaries and RTL layout.
- Independent daemon health, regular-service replica deficits, backup metadata,
  explicit expired TLS state and unavailable/unknown semantics. One-shot outcomes
  and pending task counts are not inferred from replica counts.
- Bounded fixed-query Parseable evidence with authorized inventory selection,
  time/row/byte/concurrency/deadline limits, redaction and escaped rendering.
- Ask the cluster panel and administrator provider policy form.
- OpenAI-compatible JSON Chat Completions, validated citations, no automatic
  retries/redirects, no executable model actions or browser-side credentials.
- Bounded background worker, queued jobs, owner/daemon binding and cancellation.
- Durable daemon SQLite policy/reservations (migration 0026), privileged
  accounting APIs, restart persistence and conservative unknown usage.
- Existing edge monitoring network and conditional imported/versioned provider
  key mount, without an additional service, hostname or published port.
- Earlier collector stdout attribution and separate OTLP pipeline are preserved.

The executor hit its usage quota. Primary completed the work under the user's
fallback workflow, with independent read-only review and affected-package tests.
Standalone monitoring is deployed on the three authorized hosts; no existing app deployment was changed and no paid AI calls occurred.

## Verification

- Affected packages pass: observability/Parseable, investigation/provider, UI and
  subpackages, server and store. Local fixtures exercise the full UI→accounting→
  fake-provider→polling/result flow.
- Coverage includes scope and inventory denial, malformed/empty data, bounds,
  redaction, HTML escaping, expired certificates, mutation origins/roles,
  operator accounting tampering denial, concurrent admission, dispatch replay,
  restart persistence, redirects, invalid citations and dispatched cancellation.
- Focused cluster tests pass for edge networking, optional/versioned key mounts,
  imported-key rotation, stack rendering and collector wiring.
- OpenAPI and repository documentation contract checks pass.
- Desktop, 390px mobile and Arabic/RTL Chromium checks use synthetic data: no
  page overflow, untranslated page keys, leaked fixture password or JS errors.
- Vet and daemon/edge builds pass. Independent reviewer also passed focused
  race tests and found no remaining actionable implementation findings.
- Full race suite now passes: `GOMAXPROCS=2 go test -race -p 2 -count=1 ./...`.
  The CLI test hang was reproduced and fixed by replacing real sudo/systemctl
  commands with temporary failing stand-ins. Test globals are restored; no
  production behavior changed. Focused race tests pass after the final Windows
  skip correction. Independent review found no remaining actionable findings.
  The passing suite output is saved in `docs/nextrum/verification/full-race-2026-10-17.log`.
- golangci-lint is unavailable. Docker e2e previously failed because no local
  daemon was reachable. Live Swarm validation remains unperformed; standalone Parseable ingestion
  and query behavior are now verified on all three hosts.
- The earlier full-race run in the prototype turn was stopped on user request;
  it is not a pass. Earlier standalone collector schema validation passed,
  which does not prove live ingestion.

## Live deployment — 2026-10-17

- Deployed pmcluster `observe`, native edge UI, Parseable2.8.0 and OTel0.157.0
  as the separate `nextrum-monitoring` Compose project on deshli, crestline and
  damarkets. Each has an independent backend, credentials and persisted state.
- User explicitly chose additive deployment, then requested both pmcluster and
  monitoring. No Swarm initialization, migration, app restart, Traefik change,
  or replacement of damarkets' existing monitoring.
- Containers grouped by Compose identity; unlabeled containers supported;
  ambiguous flattened identities fail closed. Desired replicas remain unknown.
  Workload mutations are disabled, including restart/exec/websocket/storage.
- Live checks passed all hosts: real stdout marker under correct service,
  host/container metrics, native page evidence, auth denial and mutation denial.
  All62 originally running containers remained running (41/6/15).
- One-day typed-stream retention configured; user cron checks actual backend
  volume filesystems and pauses only the new collector below10GiB free.
  This is a periodic guard, not a hard filesystem quota.
- Consoles bind127.0.0.1:18042 only; access via SSH. AI remains disabled.
- Crestline is now the canonical source build/test host. Source, Go caches and
  artifacts are under ~/nextrum-build. No more laptop builds for this project.
- Live testing fixed stream telemetry-type provisioning and timezone-free UTC
  timestamp redaction. Independent read-only review cleared the corrections.
  Final Crestline build, vet and full race suite passed; deployed binary hashes
  match on all three hosts. Logs and live reports are saved in
  `docs/nextrum/verification/`.

## Next work

1. Configure provider/key/model/consent/budget only when ready to enable AI.
2. Later: native resource charts, freshness, baselines, deployed-code resolver,
   MCP, automated triage, notifications and external watchdog.
3. Live Swarm-specific validation remains separate; these hosts stay Compose.

See [deployment operations and rollback](../../deploy/standalone/README.md) and
[the native guide](../../pmcluster/docs/native-observability.md).
