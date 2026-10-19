# Three-server observability

The combined dashboard is served by Deshli's existing authenticated edge at
`https://pmcluster.deshli.site/web/observability`. Overview, Logs, Metrics and
Traces default to all three nodes, with a persistent node selector for detail.
The `/web/fleet`, `/web/` and `/web/overview` entry points show the same experience.
Each host retains its own observer, collector, Parseable database and credentials.
This deployment does not initialize Swarm or change application workloads.
Deshli's public edge runs `nextrum-observability-20261019-4`; the observers and
private edges retain `nextrum-fleet-20261019-2`. See the
[fleet verification](../../docs/nextrum/verification/fleet-2026-10-19/README.md) and
[unified Observability verification](../../docs/nextrum/verification/observability-2026-10-19/README.md).

The filtered Metrics empty-node bug and standalone container metric normalization
are fixed on all three hosts; see the [follow-up verification](../../docs/nextrum/verification/observability-filter-2026-10-19/README.md).

## Placement and access

| Host | Processes | Access from the combined console |
| --- | --- | --- |
| Deshli | Host user-systemd observer and edge; Compose collector and Parseable | Local observer 9090 and backend 18080 |
| Crestline | Compose observer, edge, collector and Parseable; source build environment | SSH forwards on Deshli 19190 / 19180 |
| Damarkets | Compose observer, edge, collector and Parseable; existing monitoring retained | SSH forwards on Deshli 19290 / 19280 |

Crestline and Damarkets publish their observer at `127.0.0.1:19090` and
Parseable at `127.0.0.1:18080`. Their private host consoles remain at
`127.0.0.1:18042`. All collectors accept OTLP inside their Compose network;
this rollout publishes no OTLP receivers. Application instrumentation requires
an explicitly configured route to its local collector.

Deshli runs enabled user units `nextrum-fleet-crestline.service` and
`nextrum-fleet-damarkets.service`. User lingering is enabled. The units use
keepalives, restart automatically, bind forwarding listeners to loopback, and
pin SSH host keys. Their dedicated key is restricted to the two monitoring
destinations and cannot run commands. It is not the operator's SSH key.

The local-store backends currently use these pinned Parseable hostnames; keep
each value unchanged across a backend recreate:

| Host | Pinned Parseable hostname |
| --- | --- |
| Deshli | `46fe8a5ace7f` |
| Crestline | `ff9de8f45b39` |
| Damarkets | `28227f3ad801` |

## Configuration and operation

`OBSERVABILITY_FLEET_CONFIG` in Deshli's edge environment points to
`/home/tarek/nextrum-fleet/hosts.json`, a mode-0600 JSON array. Each entry has
`name`, `hostname`, `daemon_url`, `daemon_token`, `parseable_url`, and
`parseable_auth`. Credentials stay on the servers; never put this file in Git.
The file is read at edge startup. Update the paired endpoints and credentials
together, then restart the edge. The fleet does not use the local console's
daemon URL/token overrides in Settings.

Never shell-source a deployment `.env`: `PARSEABLE_AUTH` contains a space and
the file contains credentials. Use Compose's env-file handling or a parser that
keeps values in memory and never prints them.

The page combines bounded log counts, error-log ratios, trace counts, host
CPU/memory/load and container metrics. Detailed logs, metrics and traces are
grouped by node; filters, pagination, live polling and trace drill-down retain
that identity. Service selectors are resolved against the selected node's
authorized inventory. All hosts use one initial UTC query window. Unavailable
and partial coverage are explicit. Logs are bounded pages, not a complete export.
Investigation routes are disabled in fleet mode; no merged or remote evidence
is silently sent to the local investigation service.

Recent-log badges show stored severity. Docker stderr can therefore carry an
ERROR badge even when the application message says `level=info`; aggregate
error-log ratios use the existing message classification. Normalizing severity
at ingestion remains separate work. Traces can be listed and opened per node;
spans from different backends are not joined into a cross-node waterfall.
Existing application tracing destinations,
including Damarkets' Tempo pipeline, were preserved rather than redirected.

From an SSH session on Deshli, set the user-service bus if it is not inherited:

```sh
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
systemctl --user status nextrum-fleet-crestline nextrum-fleet-damarkets
systemctl --user restart nextrum-fleet-crestline nextrum-fleet-damarkets
systemctl --user restart pmcluster-edge
```

On each host, run the installed regular checker:

```sh
cd ~/nextrum-monitoring
python3 check-health.py
```

Stream retention remains one day and is verified by reading all three policies.
The five-minute disk guard checks actual backend volume filesystems and stops
only this deployment's collector below 10 GiB free. It requires manual recovery
after storage has been inspected: `docker compose start collector`.

### Parseable backend recreate recovery

Parseable v2.8.0 derives manifest filenames from its process hostname. Before
recreating a backend, pin the Compose `parseable` service hostname to the
original value recorded in that stream's first manifest. If a recreate already
introduced a second hostname, stop only this deployment's collector and
Parseable, make sure the backend volumes remain mounted, and run
`repair-parseable-history.py` in a temporary Python container. The tool is
dry-run by default; its apply mode requires `--offline`, creates complete
mode-0600 data and staging archives, rebuilds all affected Parquet metadata,
and leaves old files in place for rollback. Do not use `docker compose down -v`.

The repair is needed because the v2.8.0 catalog can retain a day snapshot
entry for the old hostname while repeatedly rewriting a new-host manifest.
See the upstream [`handle_existing_partition`](https://github.com/parseablehq/parseable/blob/v2.8.0/src/catalog/mod.rs#L265)
and [`manifest_path`](https://github.com/parseablehq/parseable/blob/v2.8.0/src/storage/object_storage.rs#L1463)
implementations. The tool treats a missing Parquet min/max as safe only when
the row group explicitly reports `null_count == num_rows`; an absent column is
also all-null. Any other unknown or conflicting statistics fail closed before
the backup/write phase. Do not weaken this check or clear statistics globally;
investigate the affected files and select a separately reviewed recovery plan.

After the repair, pin the stable hostname before starting Parseable and the
collector again. Check the one-day retention and fresh metrics normally, then
run the optional historical check when the stream is expected to contain older
rows:

```sh
python3 check-health.py --history-minutes 15 --history-stream metrics --history-stream logs
```

The recovery checker reports counts and metadata status only; it never prints
telemetry rows or credentials. A mixed column-statistics audit fails closed
before any write, including when only some non-null row groups lack bounds. The
all-null exception is based on Parquet null-count metadata, not inference.

The completed recovery archives are local, mode-0600 rollback inputs (and are
not an off-host backup policy):

| Host | Full data/staging archive |
| --- | --- |
| Crestline | `/home/deploy/nextrum-rollbacks/20261019-parseable-history/parseable-history-20261019T131837Z/` |
| Deshli | `/home/tarek/nextrum-rollbacks/20261019-parseable-history/parseable-history-20261019T133250Z/` |
| Damarkets | `/home/tarek/nextrum-rollbacks/20261019-parseable-history/parseable-history-20261019T133756Z/` |

After repair, each host's dry-run reported no partitions requiring repair and
the pinned hostname was used for the recreated Parseable container.

## Failure and rollback

If a tunnel fails, the combined page marks that host unavailable while the
host continues collecting locally. If Deshli is unavailable, local collectors
and backends on the other hosts still run; their private dashboards remain
available through SSH. This is not an HA presentation service or replicated
telemetry storage. Export queues are bounded and memory-backed, so prolonged
backend outages can lose records. Standard rollout rollback snapshots below do
not back up telemetry history; the three recovery archives above are explicit
local telemetry rollback inputs, and an off-host telemetry backup policy remains
separate work.

Pre-rollout configurations and binaries are under
`~/nextrum-rollbacks/20261019-observability` on each host. The previous private
container image is tagged `nextrum-pmcluster:rollback-20261019`. On private hosts,
restore the saved Compose/env/collector files, select that rollback image, and
recreate only the monitoring services. On Deshli, restore the saved public
start scripts/environment and restart its observer/edge user services. Stop
the two fleet tunnel units when removing the fleet. Preserve all named volumes;
never use `docker compose down -v`. Restoring an old configuration file alone
does not undo a database migration; consistent daemon snapshots are retained
with the rollout artifacts.
The immediately preceding fleet release has additional rollback files under
`~/nextrum-rollbacks/20261019-ui2`.
For only the unified public UI, use the startup/environment backup at
`~/nextrum-rollbacks/20261019-observability3` and restart only
`pmcluster-edge.service`; the [UI report](../../docs/nextrum/verification/observability-2026-10-19/README.md)
has the exact procedure. Preserve backend hostname pins during any rollback.

Build and test on Crestline, not the laptop. This rollout's complete source,
checks and release artifacts are under `~/nextrum-build-fleet-20261019`.
The later unified public UI source/checks/artifacts are under
`~/nextrum-build-observability-20261019`.
