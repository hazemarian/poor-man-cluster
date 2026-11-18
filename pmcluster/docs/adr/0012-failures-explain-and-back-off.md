# 0012 — A failure says what is wrong, and an unchanged stack retries on a backoff

**Status:** accepted · 2026-11-18

## The decision

Two rules about deploy failures, both enforced in `internal/stacks`:

**1. A failure names the cause, not the mechanism.** A timed-out health wait reports the services
that are NOT healthy and the evidence for each one — replica counts and the newest task that
actually failed, by state (`failed`/`rejected`), quoting its exit code:

```
timed out after 2m0s waiting for web to become healthy — web: 0/1 running, task failed (exit 1)
```

It never names the `depends_on` level ("waiting for depends_on level [db, web]"), the state-machine
word from Docker's task `error` field ("`starting`"), or a healthy sibling.

**2. A failed stack retries on a backoff, on the same revision.** 2m, 4m, 8m, 16m, then 30m, capped.
The delay is cleared by a success or by a **new revision**; recording the failure itself does not
clear it. Retries of an unchanged stack reuse the current revision, and identical consecutive
failures collapse into one history entry with an attempt count.

## Why

Both rules were paid for in on-call time on this cluster.

The message above cost hours of the wrong investigation. `depends_on level [db, web]` reads as "a
dependency is broken" — on a stack where `web` declared no `depends_on` at all, shared level 0 with
a healthy `db`, and was exiting 1 with `registering the Telegram webhook: 404`. The level is how the
deploy ORDERS services (a topological sort); it is not a diagnosis. The same message went through
three wrong shapes before it was right — `last failed task ready ("prepared")`, then
`task failed: "starting"` — because Docker fills a task's `error` field with transition words as
well as real reasons. Selecting by state and quoting the exit code is what finally survived contact
with production.

The backoff existed for the opposite reason: there was none. A failing stack was re-deployed on
every reconcile tick and each attempt blocked up to two minutes on health waits — **58 attempts on a
single revision**, measured. The first implementation of the backoff was also wrong in a way only
production showed: its "the operator touched this, retry now" escape hatch keyed on the stack row's
`updated_at`, which recording the failure itself bumps, so every retry cleared its own delay and the
ladder never climbed (three passes in a row logged `retry_in=120000`).

## What was rejected

- **Reporting everything, letting the operator filter.** Tried: the deploy error carried the level,
  the service list and the task state, and it still buried the answer. A failure message has one job.
- **Retrying harder / faster.** The platform's habit of trying again immediately is what made a
  single broken secret look like twenty broken deploys.
- **Counting `updated_at` as operator intent.** Cheap to write, false in fact — the platform writes
  that column every time it records a failure.
- **A per-stack retry setting.** Not yet: 2m → 30m covers a cold database and a genuinely broken
  stack. `health_timeout` is the knob that exists for the legitimate slow case.

## What it costs

- A stack the operator fixes WITHOUT deploying (an external service comes back) waits out the
  current delay, up to 30m, before the platform notices. The console's "retry now" and any deploy
  clear it.
- The console shows one entry with an attempt count instead of a timeline of every attempt; the
  revision history is where the detail lives.
- Every new failure mode must be phrased as a cause, which is more work than naming the stage that
  failed.
