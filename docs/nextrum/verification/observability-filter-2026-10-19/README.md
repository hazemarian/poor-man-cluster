# Filtered observability and standalone metric normalization — 2026-10-19

Deployed at https://pmcluster.deshli.site/web/observability. The reported Metrics
URL (`node=` empty, `service=svc-452cb0d9c5210dbe`, `range=1h`) now renders all
three hosts with CPU, memory, load and container metrics. Two consecutive
checks returned no partial-coverage banner, no unavailable section and no
false empty-node message. See `live-after.jsonl`.

## Causes and fixes

1. Federation validated a service selector before reading metrics. A selector
   belonging to Deshli caused Crestline and Damarkets to return after inventory
   discovery, leaving their resource cards empty. Service validation now applies
   to Logs/Investigate; irrelevant selectors do not suppress Overview/Metrics.
   Regression tests reproduce the old rendered failure and verify the fix.
2. The Docker stats receiver promotes configured labels to resource attributes.
   The standalone transform read datapoint attributes only, so the backend never
   received the common `service.name` field. Container aggregation returned HTTP
   400 on all three hosts. The transform now preserves explicit datapoint names,
   copies explicit resource names, derives resource Compose names, then supports
   legacy datapoint labels. Unlabelled host metrics retain no invented identity.

## Verification and deployment

- Go regression failed against the old source on Crestline, then passed with the
  fix. UI vet and race tests passed across all seven UI packages. See
  `regression-before.log`, `vet.log`, and `race.log`.
- Independent read-only reviews found no blocker in either fix.
- Synthetic OTLP fixtures ran through the actual contrib collector v0.157.0 on
  Crestline: the old rule failed resource Compose and explicit resource identity;
  the corrected rule passed all five cases, including explicit datapoint,
  legacy datapoint and unlabelled metrics.
- Collector candidate configurations validated with the deployed image on each
  host before replacement. Crestline was deployed and confirmed to return
  container rows before Deshli/Damarkets rollout. Only collectors were recreated;
  Parseable, daemon and edge containers were unchanged by that rollout.
- `deployment.json` records collector receipts and confirms all 62 original
  application containers retained their IDs/start times (6 Crestline, 15
  Damarkets, 41 Deshli). Backend hostname pins remain unchanged.
- `live-after.jsonl` checks the exact reported URL twice and requires all three
  hosts online, one node card with three resource values, container rows,
  zero unavailable sections and no partial banner.
- `browser.json` records the broader authenticated four-tab, node scope,
  host-control isolation, filter retention, Fleet alias, unique control IDs,
  mobile RTL and browser-error checks. Fault injection was not repeated for
  this follow-up; it is covered in the prior unified UI verification.

Public edge release: `nextrum-observability-20261019-4`.
Edge SHA-256: `40e70aed22190b69bcd9973fc84f49284512b83cb643d6ebe51e012291350223`.
Standalone collector config SHA-256:
`73dfeb6a9e5ba8bc932f3c1e052d2091bfd21f1de8fec6ae023506818b56f84f`.
Build/test source and logs remain on Crestline under
`~/nextrum-build-observability-filter-20261019/`. No local Go build/test was run.

## Rollback and scope

Public startup/environment backup: Deshli
`~/nextrum-rollbacks/20261019-observability4/`. Prior collector config on each
host: `~/nextrum-rollbacks/20261019-metric-resource-labels/otel.yaml`.
Collector rollback requires recreating only the collector with its prior config.
Do not restart Parseable or remove telemetry data to roll back these fixes.

This is a combined monitoring view of three standalone servers; it does not
create a workload scheduling cluster. Existing app workloads, Swarm configuration,
AI-disabled behavior and telemetry history are preserved. Historical metric rows
may lack the newly normalized service field; fresh metrics now supply it.
External alert delivery, application trace instrumentation and periodic off-host
backup policy remain separate work described in the existing handover/backlog.
