# Three-server fleet verification — 2026-10-19

Deployment: `nextrum-fleet-20261019-2`.
Console: https://pmcluster.deshli.site/web/fleet (existing login).

| Host | Original running application containers preserved | Deployment |
| --- | ---: | --- |
| Deshli | 41 | Existing authenticated host observer/edge; Compose collector/backend |
| Crestline | 6 | Private Compose observer/edge/collector/backend; build host |
| Damarkets | 15 | Private Compose observer/edge/collector/backend; existing monitoring retained |

All 62 original container IDs, running states and start timestamps match the
pre-rollout snapshot. All hosts remain outside Swarm. Docker is enabled at
boot; clocks report NTP synchronization. Application logging drivers are all
`json-file`, matching the collector configuration. No server reboot was used.

## Source and checks

Source is `poor-man-stack`, `nextrum/observability-console-v3`, based on
`f047df6` plus uncommitted fleet/metric changes. This is a locally built
release identifier, not a published upstream Git tag.

- Crestline initial build: `go vet -p 3 ./...`,
  `go test -race -p 3 -count=1 ./...`: 44 packages passed, zero failures/races.
  See `race.log` and `vet.log`.
- Final visual corrections: `go vet -p 3 ./internal/ui/...` and
  `go test -race -p 3 -count=1 ./internal/ui/...`: all seven UI packages passed.
  See `ui-race.log`. The final correction changed templates, CSS and UTC labels;
  the full suite was not rerun after that visual correction.
- Both final static linux/amd64 binaries and the private deployment image were
  built on Crestline. Live process/container SHA256 hashes on all three hosts
  match `SHA256SUMS-final`.
- Independent source review covered authorization before telemetry, explicit
  host pairing, endpoint/response/time bounds, credential redaction, partial
  failure, replica identity and the visual correction. No remaining blocking
  finding in that scope. This was not a whole-platform security audit.
- One reviewer also ran an earlier focused local Fleet test; release builds
  and the authoritative full/final UI checks ran on Crestline.

## Live acceptance

- Every host: observer health, correct standalone hostname/inventory,
  anonymous API rejection (401), workload mutation rejection (405), fresh
  numeric host/container CPU and memory, and all four local UI tabs passed.
- Logs, metrics and traces retention policies were read back as one day on all
  hosts. Collector configuration validates against the deployed binary.
- Unique stdout markers arrived with the intended service identity. A marker
  emitted while the collector was stopped arrived exactly once after restart;
  the previous marker was not replayed. This validates the hot-window offset
  behavior; it does not by itself validate backend-recreate history. Temporary
  probe containers were removed.
- Synthetic OTLP spans reached each local Parseable backend. Applications were
  not automatically instrumented or moved to new tracing destinations.
- Exactly one five-minute disk guard cron entry per host; checks against real
  backend volume filesystems succeeded. Credentials remain mode 0600.
- Both fleet user units are active and enabled on Deshli; user lingering is
  enabled. Dedicated keys cannot run commands or forward to unapproved local
  destinations. Host keys are pinned. Peer checks received no monitoring HTTP
  response over the tested public addresses; published endpoints bind loopback.
- Deshli's local console settings still pair its own observer and backend.
  Fleet uses separately constructed clients and ignores those Settings overrides.
- Fault test: stopping the Damarkets link yielded two of three hosts, explicit
  partial totals and two healthy host charts in 2.19 seconds. Restart restored
  three of three. Collection continued locally throughout.
- Final public browser check: existing login, three live host charts, host
  filter, aligned desktop cards/five log columns, expandable previews, mobile
  Fleet navigation, Arabic RTL, no document overflow, zero page script errors.
  Screenshot captures were used during the browser review but are intentionally
  kept out of this source repository as binary workstation artifacts.

## Parseable recreate recovery

During post-rollout verification we found a Parseable v2.8.0 local-store
catalog behavior: after a backend recreate with a new process hostname, the
day snapshot could retain the old manifest entry while later flushes wrote a
second hostname's manifest. The source paths reviewed were
[`handle_existing_partition`](https://github.com/parseablehq/parseable/blob/v2.8.0/src/catalog/mod.rs#L265)
and [`manifest_path`](https://github.com/parseablehq/parseable/blob/v2.8.0/src/storage/object_storage.rs#L1463).

All three monitoring backends were stopped with their collectors, fully backed
up, and repaired from Parquet metadata. The recovery retained every physical
Parquet file exactly once in one canonical day manifest, preserved stream
settings, and left the old files in place for rollback. A null-aware metadata
audit counted an all-null `body_userId` row group only when Parquet explicitly
reported `null_count == num_rows`; unknown or conflicting bounds would have
failed closed before any write. A Crestline ephemeral `python:3-slim` + pyarrow
fixture also verified that an all-null row group does not erase a valid range,
while a non-null file with statistics disabled is rejected.

| Host | Stable hostname | Recovery archive | Post-repair result |
| --- | --- | --- | --- |
| Crestline | `ff9de8f45b39` | `/home/deploy/nextrum-rollbacks/20261019-parseable-history/parseable-history-20261019T131837Z/` | All four streams canonical; zero orphan/missing/duplicate files |
| Deshli | `46fe8a5ace7f` | `/home/tarek/nextrum-rollbacks/20261019-parseable-history/parseable-history-20261019T133250Z/` | Eight affected partitions canonical; dry-run clean |
| Damarkets | `28227f3ad801` | `/home/tarek/nextrum-rollbacks/20261019-parseable-history/parseable-history-20261019T133756Z/` | Four affected partitions canonical; dry-run clean |

The historical checks then passed on all hosts with rows at least ten minutes
old. Crestline's independent query retained the original two-row fleet log
marker and observed 10,292 metrics rows from 09:00–09:10 UTC after repair; the
Damarkets check reported 784 metrics and 803 logs rows, and Deshli reported 566
metrics and 108 logs rows for its bounded history windows. Each backend was
recreated again with its pinned hostname and a subsequent dry-run reported no
partitions requiring repair.

Independent final queries after more than two minutes of continued collection
kept the following fixed-window counts unchanged. Metadata inspection found
one canonical manifest per stream/day and every physical Parquet file referenced
exactly once on all three hosts, with zero orphan, missing or duplicate references.

| Host | Original smoke log records | Metrics, 09:00–09:10 UTC |
| --- | ---: | ---: |
| Crestline | 2 | 10,292 |
| Damarkets | 2 | 17,680 |
| Deshli | 2 | 38,200 |

The final public browser check passed again after recovery: authenticated access,
three online hosts/charts, host filtering, desktop alignment, mobile Arabic RTL,
no horizontal overflow, and zero page script errors. The final application
baseline check still matched all 62 original IDs, running states and start times.
Screenshot captures were used during the browser review but are intentionally
kept out of this source repository as binary workstation artifacts. Visible
chart gaps in those captures correspond to the monitoring pauses used for
offline backup and repair.

Crestline's Parseable credential was rotated after the recovery. An old auth
value appeared in tool stderr during an earlier setup attempt; it is omitted
from this report and was rotated out. The old pre-rotation credential was then
rejected (`403` from the authenticated about endpoint) and the new credential
was accepted (`200`). The paired Deshli fleet entry was updated atomically.
These local recovery archives are rollback inputs, not an off-host telemetry
backup policy.
Archive sizes and SHA256 checksums are saved in `history-backups.json`.

Per-host final receipts are also stored remotely at
`~/nextrum-monitoring/verification-20261019-final.json`. Rollback locations,
operations and remaining limits are in the [fleet runbook](../../../../deploy/fleet/README.md).
