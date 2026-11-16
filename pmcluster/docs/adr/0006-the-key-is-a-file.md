# ADR 0006 — the encryption key is a file, not a channel

**Status:** accepted (2026-11-15) · supersedes the key half of ADR 0003 · amends ADR 0005 §6

## Context

ADR 0003 split the control-plane's state into a ciphertext blob and an encryption key, so that
one object would never carry both. ADR 0005 removed the blob: the mTLS mirror carries the whole
control-plane document and adoption copies nothing. The key was left behind "for now" as a
`pmcluster_state_key_<ts>` Docker config, republished on promotion and on a five-minute loop,
pruned to two — on the theory that a manager joining a cluster needed a way to obtain it.

That left the platform writing exactly one Docker config, forever, to move 32 bytes that never
change. It was also the last thing contradicting a rule the operator had already set: **no
platform secrets in Docker configs**. Three problems with keeping it:

1. **It is a delivery channel for something that does not move.** The key is minted once at
   `cluster init` and never rotates. Publishing it on a timer is machinery whose only behaviour
   is "compare, and do nothing".
2. **It is a config, so it is in Raft, in `docker config ls`, and in every backup of the swarm's
   membership** — replicated secrets for a value that is one file on one host.
3. **The comparison it enabled was a proxy.** `checkClusterKey` compared the local file's bytes
   with the published config to answer "can this node read the cluster's secrets". The real
   question is answerable directly: decrypt something.

## Decision

**The key is a file at `<data dir>/.encryption_key`. Nothing publishes it.**

- Minted by the node that initialises the cluster (`credentials.Open`), mode 0600, 32 bytes.
- Delivered to another manager by the **join route**: `POST /internal/api/join/key`, to be
  served on the swarm-mTLS listener behind `swarmtls.Verifier` to a caller presenting a manager
  node certificate. It is idempotent and has no "already served" state — a manager that lost
  its key, or joined before the route existed, asks again and gets the same bytes. No re-join,
  no rotation, no `409`. **That route is not built yet**; it is the next slice.
- Until it is, the operator copies the file by hand: same path, same bytes, `0600`. This is
  what every manager does today, and it is why the leadership WARN and `doctor` name the path.
- **A node that already holds `data.db` never mints a key.** `credentials.OpenExisting` fails
  instead. This is the safety property the whole design rests on: the previous behaviour
  (create on absence) would have let a manager that never received the key invent one silently,
  encrypt every new secret with material no other manager holds, and leave the mirrored
  ciphertext undecryptable on that host forever.
- The key is proven on a leadership gain by **decrypting a bounded sample of the store's
  secrets** with the loaded cipher. A key that opens nothing is a WARN with the file path in
  it — never a refusal, because a cluster with no leader is a worse outcome than a leader whose
  secrets it cannot read, and the operator can fix the file in seconds.

## Consequences

- The platform writes **no Docker config whatsoever**. `PublishKey`, `KeyLoop`, `newestKey`,
  `ErrNoKeyConfig`, the prune and the key label family are deleted; `internal/controlplane`
  keeps the adoption check and the two paths.
- A new manager needs one thing before it can decrypt secrets: the key file. Until the join
  route is wired, that means an explicit copy, and `doctor`/the leadership WARN name the path.
- Losing every copy of the key is unrecoverable for the secrets it encrypted — unchanged from
  before, but now there is no second copy in Raft. Nothing in the platform depends on that
  copy; it was only ever a delivery mechanism.
- The e2e helper that removes leftover `pmcluster_state*` configs stays: swarms upgraded from
  an older build still carry them, and no daemon will ever write another.
