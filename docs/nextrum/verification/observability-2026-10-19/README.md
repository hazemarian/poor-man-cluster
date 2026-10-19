# Unified three-node Observability — 2026-10-19

The main authenticated page is now
https://pmcluster.deshli.site/web/observability. Overview, Logs, Metrics and
Traces default to all three nodes. A persistent node selector opens the same
views for one host. `/web/fleet`, `/web/` and `/web/overview` are compatibility
entry points to the unified page.

## Deployed scope

Only Deshli's public edge was updated to `nextrum-observability-20261019-3`.
Its final executable SHA256 is:

```
51c6bb375dc0cdc0312f5f12a6426ce5c75e8128f455f8af606fcd6af5ca268e
```

Observer daemons and private edges retain `nextrum-fleet-20261019-2`. Collectors,
Parseable, retention policies, backend hostname pins and application workloads
were not changed by this UI rollout. All 62 original application container IDs,
running states and start times still match the pre-rollout baseline. Both
restricted fleet tunnel units are active after the fault test.

The active source is the uncommitted `poor-man-stack` checkout on
`nextrum/observability-console-v3`, based on `f047df6`. Build metadata
`f047df6-observability3` is a descriptive identifier, not a Git commit or tag.
The authoritative build source/artifacts are on Crestline under
`~/nextrum-build-observability-20261019/`. `source-final.tar.gz` captures the
final build source; its checksum is copied here.

## Behavior and boundaries

- Requests parse filters once and use one UTC window, bounded concurrent reads,
  and immutable paired daemon/backend clients. Global Settings cannot redirect
  a node's telemetry to a different inventory.
- Daemon authorization and exact configured hostname/service inventory checks
  precede telemetry. Unknown nodes fail closed. Equal service names and trace
  IDs on different hosts keep their node identity.
- Host-labelled sections preserve pagination, filter, refresh, live polling
  and trace context. Each panel has distinct control/HTMX target IDs. Form
  change events are scoped to their own form rather than every page select.
- Global cross-host credential redaction covers display text, copied service
  rows, metric/trace labels and trace details. Generated service selectors are
  opaque IDs resolved against the host's authorized inventory.
- Partial/unavailable results remain explicit. Host CPU and memory percentages
  stay per host; they are not added into a misleading cluster percentage.
- Fleet-mode investigation routes are disabled. No AI calls were made. Local
  non-fleet behavior and the existing login/role gates are preserved.

Traces are grouped and opened per node. This does not merge spans from separate
backends into one distributed waterfall. Existing application tracing routes,
one-day retention, and the lack of scheduled off-host backups/external alerts
remain as described in the fleet runbook.

## Verification

All Go builds/tests ran on Crestline, using Go 1.25 containers and persistent
build/module caches. No local Go build/test was used.

- Full `go vet -p 3 ./...` passed (`vet.log`).
- Initial full race run passed 41 packages but failed three UI packages on a
  template parse error (`race.log`). The template conditional/definition
  boundaries were corrected, including empty/unavailable service states.
- Full affected UI vet and race run then passed all seven UI packages
  (`ui-vet-final.log`, `ui-race-final.log`). Together with the unchanged packages
  in the full run, 44 distinct tested packages passed.
- Live interaction testing then exposed broad `from:select` event listeners:
  one host filter refreshed all three panels. After replacing those with
  form-local change events, the final controllers/views race checks and focused
  UI route/tab checks passed (`ui-interaction-race.log`). The final binary was
  rebuilt afterward. No additional source changes followed that rebuild.
- Independent read-only architecture/source review covered host isolation,
  authorization, concurrency, redaction, service selectors and navigation.
  The reviewer later hit its usage limit; the final template/event corrections
  were self-reviewed and verified by the tests/browser checks above.
- Final public browser checks passed login, all three nodes on all four tabs,
  node scope across HTMX tab changes, search/severity retention, exactly one
  Crestline evidence request when changing Crestline's severity, Fleet alias,
  distinct panel/control IDs, desktop/mobile Arabic RTL with no horizontal
  overflow, and zero page script errors (`browser.json`).
- Stopping only the Damarkets tunnel produced two healthy hosts plus explicit
  offline/partial coverage. Restart restored three healthy hosts. Collection
  continued locally. The test restored the tunnel in a `finally` block.
- Final application state, daemon releases, backend pins, public executable
  checksum and restored tunnels are recorded in `live-receipts.json`.

Screenshot captures were used during the browser review but are intentionally
kept out of this source repository as binary workstation artifacts.

## Rollback

Deshli's original public console startup script and environment are saved in
`~/nextrum-rollbacks/20261019-observability3/`. Restore `start-edge.sh` to
`~/nextrum-public-console/start-edge.sh` and `edge.env` to
`~/selfhost/pmcluster/edge.env`, preserving private permissions, then restart
`pmcluster-edge.service` using the user service bus. The saved startup script
points to the retained v2 edge binary. No daemon, collector or backend restart
is required. Preserve all telemetry volumes and their hostname pins.
