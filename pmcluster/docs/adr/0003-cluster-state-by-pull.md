# 0003 — Cluster state reaches other managers by pull, not by Docker config

Status: accepted (slices 1–3 landed) · Date: 2026-11-13

## Context

Today the leader copies cluster state — settings, and with them the cipher key that
decrypts secrets — to every node by writing it into **Docker configs**. Four things
follow from that, each observed rather than predicted:

- **The key lives in the replicated object.** `~/.pmcluster/.encryption_key` is material
  in a config, so anything with manager access can read what decrypts every secret value
  in the store. Encryption at rest is only as strong as the channel that carries the key.
- **Every settings write is a Raft config rotation** against Docker's 512,000-byte config
  ceiling. The control-plane survivor kit already occupies 84,740 of those bytes, and it
  grew over the cap once (stack_revisions alone reached 5.2 MB of file, 538 KB gzipped),
  which refused every publish for days.
- **A follower manager's store answers "standing by".** Its settings are not current, and
  the code paths that read settings are written as if they were. Two shipped bugs came
  from exactly that: a non-leader manager could not be selected as a runner (nothing on
  it could hear the decision), and `pmcluster doctor` cannot answer five of its checks on
  any follower.
- **Nothing verifies reach.** "The config is on every node" is a claim about a write, not
  an observation of the reader.

## Decision

**One writer, many readers that pull.** The leader's store is the only writer. Every
other node pulls the current snapshot over the swarm-mTLS channel the daemon already
serves, mirrors it into its own store, and serves reads locally **with an age**. Docker
configs stop carrying settings, and never carry the cipher key again.

The rules the code must follow:

1. **One channel, one direction.** The route lives under `/internal/api/...` on the
   swarm-mTLS listener, behind `swarmtls.Verifier`, and never on the public `/api`
   router. The caller's identity comes from its node certificate — never a query
   parameter, never a body field.
2. **Answer a projection, not the document.** What THIS node needs, keyed off that
   certificate. A worker has no store to mirror into and is refused; a manager gets the
   three mirrored tables and nothing else.
3. **The version is a content identity, not a clock.** One row, bumped inside the same
   transaction as the write it describes, stamped by the store. It is the ETag together
   with the leader's name, so a follower compares *equality* against the pair it applied.
   A numerically lower version is not "older" — a leader that adopted a control-plane kit
   answers with a version it has used before for different content.
4. **The cipher key must not come from the thing being replicated.** It is provisioned
   once at join, over mTLS, identified by the certificate, answered `409` to a node that
   already holds one, written `0600` outside the store. The snapshot carries a `key_id`
   so a node can notice its key is older than the material it is decrypting.
5. **Full snapshots, not deltas.** These tables are tens of rows, and a row absent from a
   full snapshot is deleted locally — free. A delta cannot express a delete without
   tombstones.

## What slice 1 ships (this record's claim, and its limits)

- `GET /internal/api/cluster/snapshot` on the leader's mTLS listener, managers only,
  answering `version`, `generated_at`, `leader` and `tables` for `settings`, `secrets`
  and `users`. The store type maps those names to `cluster_settings`, `secrets` and
  `users` (which is also the API-key table; `storage_nodes` is a settings key, not a
  table).
- `cluster_version` — one row, seeded by migration, bumped in the writer's transaction
  by every settings, secret and user write that actually changes something. A rewrite of
  the same value does not bump: the reconcile loop reapplies settings every pass, and a
  bump per pass would invalidate every follower's ETag forever, which is 304 forever dead.
- `If-None-Match` → `304` with no body, matching weakly (`W/`, lists, `*`).
- `cluster_version` rides the control-plane survivor kit (`store.CopyReplace`), so a
  promoted leader's version describes the data it adopted instead of restarting at 1.
- Tests: the closed table set, the 304, the write that invalidates it, a stable
  `generated_at` per version, a delete visible as an absence, empty tables as `[]` not
  `null`, the worker refusal, GET-only, 503 without a store, the leader+version ETag, the
  weak comparison, the bump rules per write path, and the route's absence from the
  public API.

**It changed no behaviour on its own.** Settings still travelled in Docker configs,
followers still answered "standing by", and nothing polled the route. Deliberate: slice 1
was additive so it could be deployed and observed before anything depended on it.

## What slice 2 ships

- A follower manager polls the route every 60 s on the runner-assignment cadence, from
  `internal/cli/snapshot_mirror.go`, started BEFORE the leadership wait (a follower blocks
  there forever) and after the startup control-plane restore (which is only safe while
  nothing has the store open). At promotion the loop is stopped and JOINED before the
  leader's store opens: this node becomes the writer, and two writers to one row is a race
  whose winner nobody can predict.
- `Store.ApplySnapshot` replaces the mirrored rows in one transaction — upsert
  `cluster_settings` and `secrets` on their natural keys, replace `users` (two unique keys
  and no local authority, so an upsert could stall on a name/id conflict), delete every row
  the snapshot does not carry, then record `sync_state`.
- `sync_state` (one row, never replicated, classified as excluded from the survivor kit)
  carries `leader`, `version`, `generated_at`, `applied_at`. `mirrored_at` on the three
  tables answers "did this row arrive, and when".
- The poll is conditional on the (leader, version) pair this node applied. A bare version
  would match another leader's identical number after a failover and answer 304 for state
  that was never applied here.
- A failed poll changes nothing: the last applied snapshot stays, the log says so.
- The leader never fetches from itself; the tick checks leadership every time.

## What slice 3 ships

- `store.MirrorReading` — leader, version, `applied_at`, and the row counts, plus one
  `Human()` label so every surface says the same sentence. **The age is measured from
  `applied_at` on this node's clock and clamped at zero**: subtracting the leader's
  `generated_at` from a local clock is the cache-report bug (fresh data read as stale), and
  a backwards clock must not print a negative age.
- `doctor` on a non-leader answers a `cluster mirror` check — OK with the age, WARN and
  `STALE` past three poll intervals, WARN when nothing has ever been applied. The old
  blanket "cluster state: not answered here" is now a specific list: secret decryption needs
  the cipher key that slice 5 provisions, and stacks/revisions, the last apply and the kit
  drill were never in the projection.
- `cluster settings` reads answer on a follower, prefixed with the label; `set` still needs
  the leader. The guard decides on the subcommand WORD, because a scope table keyed by
  command name cannot tell the read form from the write form.
- A follower MANAGER was being told the node list is a manager-only call. It is not; the
  line now differs by role.
- `clustersnapshot.PollInterval` is the channel's constant, so the loop and the staleness
  threshold share one number.

**Amended after the first deployment.** The age was computed from `applied_at`, and
`applied_at` only moves when a 200 changes rows — so on a healthy quiet cluster, where every
poll is a 304, a follower reported `STALE` four minutes after it started. Migration `0032`
adds `sync_state.checked_at` (last successful answer, 304 included) and the age is computed
from that; `applied_at` keeps its meaning as "when the rows were written" and is reported
beside it. The rule that survives: **an age may only be derived from a signal that moves
when the thing it describes is still happening.** A version number (and an ETag) move when
content changes; freshness needs a different column.

Still unbuilt: settings still travel in Docker configs (slice 4), and there is no join-time
key route (slice 5) — so a follower can mirror secret CIPHERTEXT but cannot yet decrypt it.

## Consequences

- **Costs, stated plainly.** A manager now receives, over one route, the material the
  config path already gave it — including secret **ciphertext**, never plaintext, and the
  argon2id token hashes. That is not a new class of reader; it is the same class, through
  a channel with a revocation story (`docker node rm` drops the certificate) instead of an
  object any manager can read at will. Workers are refused, so the audience narrows.
- A settings write reaches a follower within one poll interval (60 s, the runner
  assignment's), not instantly. Every read surface must therefore carry an age — a blank
  answer is not acceptable, a stale one is.
- Polling costs one request per follower per interval, and a 304 is a few dozen bytes. It
  does not scale down to nothing: the poll is the liveness signal as well as the sync.
- The mirror is a **second copy of secret ciphertext** on every manager's disk. It is the
  same copy the kit writes today; a key rotation must reach every node, which is why the
  snapshot carries `key_id`.
- `cluster_version` is now a table the schema-classification test demands an answer for,
  and the kit carries it with a selector that exists for exactly one table.

## Handoff checklist (slices 2–5)

- [x] **2 — follower mirror.** Landed: see "What slice 2 ships". One deviation from the
      brief's schema, stated here so it is not rediscovered: `mirrored_at` was also added
      to `users` (the projection carries three tables, and a mirrored row that cannot
      answer "did you arrive" is the defect the column prevents).
- [x] **3 — switch the readers.** Landed: see "What slice 3 ships". `cluster settings`
      reads and `doctor`'s cluster mirror line answer on a follower with `age_seconds` and
      the leader's name. Secret decryption stays unanswerable there until slice 5 gives the
      node a key — stated in the report rather than implied by a green tick.
- [ ] **4 — retire the config path** (irreversible, and only after 3 has been observed for
      a day). Then the size workaround that existed for the 512 KB limit goes too, and a
      metric asserts that a write reaches every manager within one poll.
- [ ] **5 — the cipher key.** `POST /internal/api/join/key`, `200` once, `409` after,
      `0600` outside the store; then rotate everything that ever lived in a config: the
      S3 access key and secret, the SSO client secret, the SeaweedFS store secret.
- [ ] Rolling upgrade: a node on the old binary must keep working throughout — the route
      is additive, and nothing may require it until the whole fleet has it.
