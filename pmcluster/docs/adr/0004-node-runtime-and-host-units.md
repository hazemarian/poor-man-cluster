# 0004 — A node's loops and its host units are not the CLI

Status: accepted · Date: 2026-11-15

## Context

`internal/cli` grew to 26,461 lines in 105 files, and it held three different jobs.
That is not a style observation — it is what three separate defects traced back to:

- **A systemd bug was hunted in the wrong package.** The daemon's own unit was rendered
  in `internal/cli/daemon.go` and the backup agent's six units in
  `internal/cli/backup_unit.go`, beside forty `cobra.Command` definitions. A `PartOf`
  versus `BindsTo` interaction and a drop-in that could not cancel one cost a session
  each, in the package that no one opens expecting to find systemd, and the two
  installers could not share the seams their tests need (`HostOS`, `Systemctl`,
  `WriteUnit`) without exporting them from a command package.
- **Every test of a node loop had to build a cobra command.** The assignment channel,
  the snapshot mirror, the volume-repair loop, the swarm leadership watch, the docker
  watchdog and the telemetry supervisor all take a context and a few collaborators, and
  none takes a `*cobra.Command`. They lived in the CLI only because `serve` did, so
  their tests constructed a command to reach a function that does not use one — the
  same shape that produced a nil-`cmd.Context()` panic inside `database/sql`.
- **The never-create rule for the store was written six times.** `store.Open` creates a
  database; that is right in `init` and `join` and wrong everywhere else, so six call
  sites hand-rolled an `os.Stat` guard with four different sentences, and a reader
  could not tell a deliberate create from an oversight.

## Decision

**Three packages, one job each, and a guard that needs no exemption list.**

1. **`internal/cli` is a surface.** A file here parses flags, prints, and calls
   something else. It is not a home for behaviour that has no command.
2. **`internal/noderuntime`** holds the loops `serve` starts. Exported entry points take
   `(ctx, cfg, collaborators)`; nothing takes a `*cobra.Command`.
3. **`internal/nodeunits`** holds everything a node installs on itself — the systemd
   seams, the daemon's unit and memory drop-in, the health watchdog, the backup agent's
   units — plus the node-local file names those units read. A constant two layers must
   agree on lives here, not in whichever package defined it first.
4. **`store.OpenExisting` is the shared never-create rule.** A package that cannot
   import `internal/cli` reads settings through it; `cli`'s opener is a one-line call.
5. **`internal/cli/boundary_test.go` enforces the split**: no top-level name may be
   declared in more than one of the three packages, and a fixed list of runtime entry
   points must be present in `noderuntime` and absent from `cli`.

## Rejected

- **"Every file in `internal/cli` defines a command."** It was the first guard written
  and it does not survive contact with the code: eleven files are legitimately not
  commands (`store_open.go`, `swarm.go`, `tailscale.go`, `telemetry.go`,
  `storage_converge.go`, …), so the test would ship an eleven-entry exemption list. An
  exemption list nobody re-reads is how a boundary re-erodes. The two rules that
  replaced it need no list and were each proved against a deliberately broken tree.
- **A single `internal/host` package for units AND loops.** They have different
  dependencies: the loops want the store, docker and the API client; the unit renderers
  want strings and a `systemctl` seam. Merging them drags the store into the unit tests.
- **Exporting test support from production files** so a moved test could keep using
  `logCapture` and `leaderFake`. A `_test.go` is not importable, and a fake does not
  belong in the binary; the two fakes were duplicated into the destination packages.
- **Reaching for the CLI's package-level `configPath`** from a moved function. It is now
  a parameter (`EnsureDaemonRunning(out, configPath)`), so "which node's config" is
  answerable from the signature.

## What it costs

- Two package boundaries to keep clean by hand — mitigated by `boundary_test.go`, which
  is cheap and fails loudly.
- Two copies of a few test helpers (`logCapture`, `leaderFake`, `unitSections`,
  `unitSeconds`). A fake duplicated is noise; a fake shared through production code is
  a defect.
- Exported names that exist only for the CLI (`DaemonUnitPath`, `StopUnitDropIn`,
  `MemoryLimitsFor`) are settable package state. That is the same shape as before, moved
  to a package where the seams are the point.

## Evidence it was worth doing

`internal/cli` was 105 files / 26,461 lines; it is 45 files / 19,307, with `noderuntime`
at 5,012 and `nodeunits` at 2,698. Its `-race` suite went **79.8s → 15.4s**, because the
loops that wait minutes took their waits with them. Nine non-command files remain in the
CLI — `store_open.go` (the openers commands share), `telemetry.go`, `swarm.go`,
`tailscale.go`, `github_registry.go`, `registry_replay.go`, `drill_status.go`,
`storage_converge.go` — each a helper its commands call, which is why the guard does not
try to outlaw them. The build, the vet pass
and the lint are unchanged in shape; the only behavioural change in the whole move is
that an open racing a deleted file now refuses instead of recreating it.
