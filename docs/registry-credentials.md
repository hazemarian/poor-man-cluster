# Registry credentials (private image pulls)

pmcluster pulls its images from a private registry (ghcr.io). The credential
that makes those pulls work lives in **pmcluster's encrypted control-plane
store**, is replayed into Docker on the leader at daemon start, and reaches
worker nodes through `docker stack deploy --with-registry-auth`. This document
is the contract: what the mechanism is, what a correct credential looks like,
and how to rotate it without ever taking the cluster's pulls down.

Every behavioural claim here is traceable to the implementation; file:line
citations are against the repo at time of writing.

Sections: [1 where the credential lives](#1-where-the-credential-lives-and-why) ·
[2 registering one](#2-registering-a-credential) ·
[3 what each command does](#3-what-each-command-does-line-by-line) ·
[4 the credential spec](#4-the-credential-spec-ghcrio) ·
[5 how it reaches workers](#5-how-the-credential-reaches-workers) ·
[6 rotation runbook](#6-rotation-runbook) ·
[7 failure modes](#7-failure-modes) ·
[8 cadence](#8-rotation-cadence) ·
[9 the GitHub App alternative](#9-the-github-app-alternative)

> **The preferred credential is now a GitHub App installation token.** When one
> is configured (`pmcluster github app set`), the daemon logs in to ghcr.io with
> it and skips the stored `ghcr.io` row; when it is absent or its login fails,
> the stored credential below is replayed exactly as before. Other registry
> hosts are unaffected. The App path — and the CI token broker — is documented in
> [`github-app.md`](github-app.md); §9 here records precisely what changes.

---

## 1. Where the credential lives, and why

Three mechanisms could carry a registry password into a swarm. Only the first
is a pmcluster-managed one, and the reason it wins is *where the bytes end up
and who controls them*:

| Mechanism | Where the value actually lives | Consequence |
|---|---|---|
| **pmcluster store** — `pmcluster registry add` | `~/.pmcluster/data.db`, `registries` table, `password_ciphertext BLOB` (`pmcluster/migrations/0004_webhooks_registries.sql:21-26`; `data.db` path: `pmcluster/internal/config/config.go:32-33`, default DataDir `~/.pmcluster` at `config.go:49`) — AES-256-GCM, key in `$DataDir/.encryption_key` mode 0600 (`pmcluster/internal/credentials/cipher.go:1-6,94`) | One authoritative copy, survives a manager rebuild, replayable, never in a plaintext file you manage |
| **A manifest / compose field** | plaintext YAML, committed or shipped to the control plane | The DSL has **no** field that carries a registry credential — `registry:` in a manifest is only an image-name prefix with `${registry}` substitution (`docs/dsl.md:22,309,313`). There is nothing to accidentally leak because there is nowhere to put it. |
| **A hand-edited `~/.docker/config.json`** | plaintext (or base64 — *encoding, not encryption*) JSON on whichever node you edited, invisible to pmcluster | It is what Docker itself reads, so `registry add` also writes it — via `docker login`, not by hand (`registry.go:193-199`, help text `registry.go:23-26`). A *hand-edited* copy, though, drifts: it is not in the DB, so a manager rebuild's replay pass (`registry_replay.go:17-20`) will not restore it, and nothing lists or removes it atomically. |

The design in one sentence, from the command's own help: pmcluster
*"Stores per-host credentials encrypted in SQLite AND runs 'docker login' so
they land in `~/.docker/config.json`. 'docker stack deploy
--with-registry-auth' then forwards the manager's auth to every node so
workers can pull private images without their own login."*
(`pmcluster/internal/cli/registry.go:23-26`; same contract in the migration
comment `migrations/0004_webhooks_registries.sql:8-11`).

Two copies exist by design — the encrypted DB row (durable truth) and the
leader's `~/.docker/config.json` (what Docker/deploy actually uses) — and
`registry add` writes both in one step so they cannot silently disagree. A
third place exists only for convenience: a worker's own `config.json`, copied
there on join (§5.2).

> **Run registry commands on the leader node itself.** The whole `registry`
> command group is classified `scopeLeader`
> (`pmcluster/internal/cli/leader_guard.go:74`): on a follower the CLI refuses
> *before* opening the store (`leader_guard.go:135-157`), and off-Docker
> machines get an explicit refusal. `--api-url` skips the leader check
> (`leader_guard.go:139`), **but the registry commands still open the local
> store** — `runRegistryAdd/List/Remove` call `openStore()` unconditionally
> (`registry.go:77,118,144`) and there is no remote backend for registries
> (`internal/cli/backend.go` has remote clients for configs/secrets/webhooks,
> none for registries; verified against a fresh build: `registry list
> --api-url …` fails with `data directory not initialised`). So remote-mode
> `registry add` from your laptop would register a credential in a laptop-local
> DB and docker-login your laptop — not the cluster's. This is the one place
> where the leader-guard's remote-mode carve-out does not match the command's
> actual backend; treat `registry *` as **leader-host-only** commands.

## 2. Registering a credential

The canonical form — token piped on stdin, never an argument, never echoed:

```bash
# on the leader
cat /path/to/token | pmcluster registry add ghcr.io --username <ghcr-user> --password-stdin
# or straight from your vault, e.g.:
op item get ghcr-pull --fields one-time --reveal | \
  pmcluster registry add ghcr.io --username <ghcr-user> --password-stdin
```

(`--username` is required — `registry.go:65-67`; an empty password is refused —
`registry.go:73-75`. For ghcr.io the username is the GitHub login that owns the
token.)

The ready-made version is [`pmcluster/hack/registry-rotate.sh`](../pmcluster/hack/registry-rotate.sh):
it reads the token from stdin only (rejecting an empty or whitespace-bearing
stdin), calls `pmcluster registry add`, then prints the exact verification
commands for §6 without ever printing the token.

**Why `--password-stdin` and not `--password`:** a flag value is visible in
`ps`, in your shell history, and in any CI log that echoes the command line.
The CLI itself labels the flag *"registry password (avoid — leaks to shell
history)"* (`registry.go:55`), and `--password` and `--password-stdin` are
mutually exclusive (`registry.go:170-172`). Even with `--password`, the
password is never passed to Docker as an argument: pmcluster always runs
`docker login <host> -u <user> --password-stdin` with the value on the child's
stdin (`registry.go:193-199`). The stdin path also strips a trailing newline
for you (`registry.go:176-181`), so `echo TOKEN | …` works; a token containing
internal whitespace does not.

There is no dedicated `--token` or `--github-username` flag and no ghcr.io
special case in the CLI — the username/password pair is registry-agnostic by
design.

## 3. What each command does, line by line

### `pmcluster registry add <host>` (`registry.go:62-115`)

1. **Validate** — `--username` required, resolved password non-empty
   (`registry.go:64-75`).
2. **Open the store** — local SQLite at `cfg.DBPath()`, refusing if `pmcluster
   init` never ran (`registry.go:77` → `internal/cli/credentials.go:204-217`).
3. **Open the cipher** — `credentials.Open(cfg.EncryptionKeyPath())`; the
   32-byte key at `~/.pmcluster/.encryption_key` is created 0600 on first use
   (`registry.go:83-86`; `config.go:33`; `cipher.go:32-34,94`).
4. **`docker login <host>` first** — via `--password-stdin`; if the registry
   rejects the credential, the command aborts **before** anything is written to
   the DB (`registry.go:88-90`). Failure ordering matters for rotation (§6):
   a bad token can never overwrite a good stored credential.
5. **Encrypt and upsert** — `cipher.Encrypt(password)` → `12-byte nonce ||
   AES-256-GCM seal` (`cipher.go:50-61`), then `UpdateRegistry` if the host
   row exists, else `CreateRegistry` (`registry.go:92-111`). `host` is the
   table's PRIMARY KEY (`migrations/0004…sql:21-26`), so re-adding `ghcr.io`
   **overwrites** username + ciphertext in place — this is the rotation
   primitive.
6. **Print the propagation promise** — *"Workers will receive these
   credentials on the next deploy via --with-registry-auth"*
   (`registry.go:113`). Note the tense: nothing is pushed to workers at
   `add` time; the next deploy is what carries it (§5.1).

Output on success: `✅ Registry ghcr.io added (user: …)`, or `updated (was X,
now Y)` on overwrite (`registry.go:105,110`).

### `pmcluster registry list` (`registry.go:117-140`)

Reads the store and prints `HOST / USERNAME / CONFIGURED` — **never** a
password, plaintext or ciphertext (`registry.go:134-137`; the column header
says "CONFIGURED" because the value printed is the row's `created_at`, RFC3339).
Caveat inherited from the store layer: `UpdateRegistry` only touches
`username, password_ciphertext` (`pmcluster/internal/store/registries.go:48-53`),
so the timestamp stays at the **first** time that host was added — it does not
show when you last rotated. Keep the token's real expiry somewhere else
(§6 step 6); the list lies by omission about freshness.

### `pmcluster registry remove <host>` (`registry.go:142-163`)

Deletes the DB row (`store.DeleteRegistry`, `registries.go:102-114`), then runs
`docker logout <host>` on the leader (`registry.go:157-159` — a failing logout
is a warning, the row is already gone). Removes *both* designed copies
atomically-ish. Consequences on a running cluster: **in-flight tasks keep
running** (the image is already on the node), but the next deploy can no longer
forward auth for that host, and the next manager restart will not replay a
login for it — which is how a quiet `registry remove` turns into tomorrow's
ImagePullBackOff (§7).

## 4. The credential spec (ghcr.io)

The requirement is a dedicated credential with **read-only access to the
packages and nothing else**. Two realities from GitHub's current docs (both
pages accessed 2026-11-08) shape how that is spelled:

1. **ghcr.io does not accept fine-grained PATs today.** GitHub's Container
   registry doc says: *"GitHub Packages only supports authentication using a
   personal access token (classic)"*
   ([docs.github.com — Working with the Container registry](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)),
   and the fine-grained limitations list still names *"Using fine-grained
   personal access token to access Packages"* as an open gap
   ([docs.github.com — Managing your personal access tokens](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens);
   tracked in `github/roadmap#558`, reported open as of mid-2026). So the
   working form of "fine-grained, Packages: Read only" is a **classic PAT
   carrying exactly one scope: `read:packages`**.
2. **Package access rides on repository access.** A linked package inherits
   its permissions from its repository, and *"your personal access token
   (classic) must have the `read:packages` scope, and your user account must
   have read permission"* on the owning repository
   ([docs.github.com — About permissions for GitHub Packages](https://docs.github.com/en/packages/learn-github-packages/about-permissions-for-github-packages)).
   The token authenticates as its owner, so **the GitHub account behind the
   token must itself have read access to the repos that own the images**
   (resource owner `nextrum-sy`). That is the honest tradeoff: you cannot
   scope a classic PAT below the owner's repo visibility — if the account
   loses access to the owning repo, the credential stops working even with the
   "right" scopes; and if the account ever gains org membership, this token's
   blast radius silently grows. Prefer a dedicated low-privilege account for
   the pull credential if the org model allows it.

Spec to mint today:

- **Preferred: no PAT at all — a GitHub App installation token.** GHCR accepts
  one when the App holds **Packages: Read** and is installed on the repo that
  owns the package. The daemon mints it, uses it as
  `docker login ghcr.io -u x-access-token --password-stdin`, and refreshes it
  every 25 minutes. See [`github-app.md`](github-app.md). The rest of this
  section describes the PAT fallback, which is still fully supported.
- **Fallback type:** personal access token (**classic**) — a fine-grained
  token with "Packages: Read" fails `docker login ghcr.io` with `denied:
  denied` (fine-grained PAT support for Packages is still an open gap;
  App installation tokens are the supported fine-grained mechanism).
- **Scopes:** `read:packages` **only**. Uncheck everything else — no `repo`,
  no `write:packages`, no `delete:packages`. A `read:packages`-only token
  cannot read source code; the puller's compromise is then bounded to
  downloading the images it already has authority over. (If/when fine-grained
  works: resource owner `nextrum-sy`, repository access = only the repos
  publishing the images, permissions = Packages: Read + the always-on
  Metadata: Read, nothing else.)
- **Username for `registry add`:** the GitHub login that owns the token
  (ghcr.io requires the token's owner's username; `pmcluster registry list`
  is where that username is recorded).
- **Expiration:** set one. GitHub recommends expiry for all PATs, and notes
  unused classic PATs older than a year are auto-removed anyway (Managing
  your personal access tokens, §above) — an explicit date is your rotation
  trigger, §8.
- **If the images are ever made public:** public container images on ghcr.io
  can be pulled anonymously (About permissions for GitHub Packages, §above),
  so a private-package credential becomes unnecessary — but keeping the
  read-only token costs nothing and survives a package being flipped back to
  private mid-incident.

Sanity check that scopes are right *without* a cluster: a `read:packages`
token must pass `pmcluster registry add ghcr.io …` (which is a `docker login`)
but a push with it must be denied. Do **not** test that on a real tag — push
permission is the CI `GITHUB_TOKEN`'s job, never the puller's.

## 5. How the credential reaches workers

### 5.1 Deploys: `--with-registry-auth`

Every pmcluster stack deploy runs:

```
docker stack deploy --detach=true --resolve-image=always --with-registry-auth -c - <name>
```

(`pmcluster/internal/cluster/stacks.go:81-89`). `--with-registry-auth` takes
the **manager running the deploy**'s `~/.docker/config.json` entry for the
image's registry and attaches it to the service's task spec, so the swarm
agents on workers pull with it — no worker-side login needed. `pmcluster
deploy` and `pmcluster cluster update` both route through this deployer
(`pmcluster/internal/stacks/deploy.go` uses the injected `Deployer`, built in
`pmcluster/internal/cli/deploy.go:157`; `cluster update` calls
`DeployStack` per stack, `pmcluster/internal/cluster/update.go:937`).

Two sharp edges:

- **A deploy is the delivery event.** After rotation, the new credential is
  live for scheduled services only once a `pmcluster deploy`/`cluster update`
  has run (`registry.go:113`'s "next deploy"). A bare `docker service update
  --force` reuses the credential already embedded in the service spec
  (`stacks.go:241-245` sends no `--with-registry-auth`), so "force" restarts
  do **not** pick up a rotated token — go through pmcluster's deploy.
- **`--resolve-image=always`** means the deploy asks the registry for the
  current digest of every tag, so a bad credential surfaces as a deploy error
  on the spot, not later at pull time (`stacks.go:84`).

### 5.2 Joins: `verifyRegistryAuth` / `copyRegistryCreds` / `verifyRegistryPull`

`pmcluster join` is the only other path where per-node credentials exist.
Three best-effort helpers, never fatal to the join (`pmcluster/internal/cli/join.go:109-115,245-263`):

- `verifyRegistryAuth(out)` reads the **local node's** `~/.docker/config.json`
  and warns if it is missing or has an empty `auths` map — because a node with
  no credentials *"runs whatever image happens to be cached locally — silently
  stale"* (`join.go:391-418`, quote at 392-395). It never checks ghcr.io
  itself; presence of *some* auth is the test.
- `--copy-registry-creds <manager-ssh-host>` ssh-fetches
  `/root/.docker/config.json` from the manager (60 s timeout,
  `join.go:90,430-489`), **merges** its `auths` into the node's existing file
  (copied entries win per-registry; unrelated keys like `credsStore` survive),
  backs the old file up to `config.json.bak`, and writes the result 0600
  (`join.go:421-431,466-484`). This is why the *copy* carries the leader's
  login state — whatever `registry add`/replay put in it (§5.3) — and it is
  also a drift source: it is a snapshot at join time (§7, row 2).
- `--verify-registry-pull <image>` runs `docker pull <image>` on the new node
  (2 min timeout) and prints `✔ registry pull succeeded` / `⚠ registry pull
  failed — check credentials` (`join.go:91,541-555`). This is the join-time
  proof; §6 uses its manual equivalent as the rotation-time proof.

### 5.3 Manager restart: `replayRegistryLogins`

On `pmcluster serve` startup, the daemon calls
`replayRegistryLogins(ctx, st, cfg, log)` — before serving, right after the
store opens (`pmcluster/internal/cli/serve.go:221-223`). For every DB row it
decrypts the stored password and re-runs
`docker login <host> --username <user> --password-stdin`
(`pmcluster/internal/cli/registry_replay.go:36-51`). Purpose, from the code
comment: *"so a manager rebuild (SQLite survived, ~/.docker/config.json gone)
keeps pulling private images"* (`registry_replay.go:17-20`). Failures are
collected per-host and surfaced as one warning — *"registry re-login had
issues; private images may fail to pull"* — that never blocks the daemon
(`registry_replay.go:52-54`, `serve.go:221-223`).

Consequences worth internalising:

- The encrypted DB row, not `config.json`, is the source of truth across
  rebuilds. `docker logout` on the leader is temporary: the next serve
  replays from the DB and logs back in.
- If the **encryption key is lost** but the DB survives, replay fails at
  `cipher.Decrypt` (`registry_replay.go:37-40`) with `invalid or tampered
  ciphertext` (`credentials/cipher.go:24-25,65-75`) — the stored credential is
  cryptographically unrecoverable. Package docs say it plainly: *"losing it
  renders all encrypted credentials unrecoverable — back it up with the DB"*
  (`cipher.go:1-6`). Recovery = re-run `registry add` with a freshly minted
  token; there is no decrypt-without-the-key path.

## 6. Rotation runbook

Plan: **mint → register → deliver → prove → revoke.** The old token stays
valid the whole time; you revoke only after proof. One failure at step 2 or 3
leaves the old credential fully in force (the DB write happens only after a
successful login, §3 step 4), so the procedure is safe to interrupt until the
revocation step.

Assume ghcr.io, resource owner `nextrum-sy`. Run everything on the leader
unless a step says "on the worker".

1. **Mint the new token** at
   `https://github.com/settings/tokens/new?scopes=read:packages` —
   `read:packages` only (§4), with an explicit expiration. Note the GitHub
   login that owns it (the `--username` value, and the one already shown by
   `pmcluster registry list`).

2. **Register it** (overwrites the stored credential in place):

   ```bash
   # canonical: token from your vault / a file → stdin → helper
   cat /path/to/new-token | pmcluster/hack/registry-rotate.sh \
       --username <ghcr-user> --image ghcr.io/nextrum-sy/<app>:<tag>
   ```

   Or without the helper: `pmcluster registry add ghcr.io --username
   <ghcr-user> --password-stdin < /path/to/new-token`.
   Expect `✅ Registry ghcr.io updated (was <same user>, now <same user>)`
   (`registry.go:105`). If you instead see `docker login ghcr.io: exit status
   1` wrapping `denied: denied` — that is ghcr.io's real rejection text for a
   bad/under-scoped token (observed against ghcr.io 2026-11-08: a failed
   login prints `Get "https://ghcr.io/v2/": denied: denied`) — **the store was
   not touched**; fix scopes and redo step 1.

3. **Deliver it to services** — a deploy re-embeds the manager's fresh auth in
   the task specs (§5.1):

   ```bash
   pmcluster cluster update
   ```

4. **Prove a real pull, twice.**

   a. *The scheduler's own pull* — every task must converge:

   ```bash
   docker service ps --no-trunc <stack>_<service>
   ```

   Healthy end state: latest task `running`/`complete`. Auth-wrong signature:
   tasks cycling to `rejected`/`failed` with the agent error naming the image
   (e.g. `pull access denied for ghcr.io/…` or `error pulling image
   configuration: … unauthorized`/`denied` — exact wording varies by Docker
   version, which is why `--no-trunc` matters; and orchestrator state
   `pending` with `No such image: …` while the agent cannot fetch). Check the
   service's event stream with `docker service events <stack>_<service>`.

   b. *The worker's naked daemon* (proves the copied/embedded credential
   itself, not a cache hit). On the worker, pull **the digest the running
   service resolved to** — tags can be answered from cache, digests force
   registry contact for a blob the node doesn't have yet, so first delete the
   local copy:

   ```bash
   # on the leader: the exact repo@digest the swarm resolved
   docker service inspect <stack>_<service> \
     --format '{{.Spec.TaskTemplate.ContainerSpec.Image}}'
   # on the worker: drop the cached copy, then pull that digest
   docker rmi <image-ref> 2>/dev/null || true
   docker pull ghcr.io/nextrum-sy/<app>@sha256:<digest>
   ```

   Expected success ends with `Digest: sha256:<digest>` /
   `Status: Downloaded newer image…`; a revoked/bad credential fails with
   `denied: requested access to the resource is denied` or `pull access
   denied`. (The join-time equivalent of this check is
   `--verify-registry-pull`, §5.2; if the worker ever rejoined after a
   `config.json` wipe, that is the flag to redo.)

5. **Revoke the old token**: `https://github.com/settings/tokens` → delete the
   superseded PAT. Only after 4a **and** 4b pass. Then repeat 4b once —
   revoking the old token must not break anything any more.

6. **Record:** write the new token's **expiry date** into your rotation
   calendar / password manager entry. `pmcluster registry list` confirms the
   host+username are registered, but its timestamp is the original creation
   date, not the rotation date (§3, `list`), so it cannot be your expiry
   ledger.

Rollback if anything fails before step 5: nothing was lost — the old token is
still valid and, if you registered a bad one by mistake, re-register by piping
the **old** token through the same `registry add` (it overwrites again).
After step 5, rollback = mint a fresh token; a revoked GitHub token cannot be
un-revoked.

## 7. Failure modes

| Symptom | What actually happened | Diagnosis | Fix |
|---|---|---|---|
| Worker `ImagePullBackOff` / tasks `rejected` after a rotation | the rotated token lacks `read:packages`, belongs to an account that lost repo access (§4), or no deploy ran since registration so services still carry the revoked credential in their task spec (§5.1) | `docker service ps --no-trunc`; manual digest pull on the worker (§6.4b); `pmcluster registry list` shows what is stored | re-run §6.2 with a correct token, then `pmcluster cluster update` |
| Login fails with `Get "https://ghcr.io/v2/": denied: denied` | registry rejected the credential at `registry add` time — bad/expired/under-scoped token (or fine-grained PAT against ghcr.io, §4) | that exact text is docker's; the DB was **not** modified (login precedes write, `registry.go:88-95`) | mint per §4 and redo step 2 |
| Worker runs a stale image, no errors anywhere | node has no credentials of its own; Docker only pulls when the image is absent, so the swarm "succeeds" with a cached copy — called out as the silent-stale trap in `join.go:391-395`; the console shows `Image N days old` for it (`ui/controllers/services.go:297`) | `verifyRegistryAuth` logic by hand: does the worker's `~/.docker/config.json` `auths` contain `ghcr.io`? | rejoin with `--copy-registry-creds <leader> --verify-registry-pull <image>` (§5.2), or ensure every deploy goes through pmcluster |
| `~/.docker/config.json` on the leader disagrees with the DB (hand login, hand logout, edited file, or a join-copied worker snapshot gone stale) | the DB is the truth that replay restores (§5.3); `config.json` is the working copy; the worker copy from `--copy-registry-creds` is a **point-in-time snapshot** (`join.go:421-489`) with no refresh mechanism | `pmcluster registry list` vs `cat ~/.docker/config.json` (compare *presence*, not content — the file holds a base64 password; don't propagate it) | re-run `registry add` to rewrite both designed copies; for workers, re-copy or rely on deploy forwarding |
| Manager rebuilt; private pulls fail at start with `registry re-login had issues; private images may fail to pull` in the daemon log | replay ran but decryption failed ⇒ encryption key lost, or docker daemon wasn't up yet, or the stored token was revoked between rebuilds | log line source `serve.go:221-223` → per-host detail from `registry_replay.go:52-53` (`<host>: decrypt: …` = key problem; `<host>: exit status 1 (…denied…)` = credential problem) | key problem: mint a new token, `registry add` again (recovery path, §5.3); credential problem: check the token is still unrevoked; then confirm next deploy succeeds |
| `registry remove ghcr.io` while stacks run | row deleted + `docker logout` on leader (`registry.go:150-159`); running tasks unaffected (image already pulled); the *next* deploy forwards no ghcr.io auth and a manager restart won't replay it | `pmcluster registry list` shows it gone; deploys start failing image resolution | re-`registry add` before any deploy; treat `remove` as "unhook this registry from pmcluster", not as cleanup |
| `pmcluster registry add` on a follower/worker prints `is a worker, not the leader` or `this command needs the cluster leader…` | leader guard (`leader_guard.go:70-74,135-186`) — correct behaviour, nothing was read or written | — | run it on the leader; do **not** "fix" it with `--api-url`: registry commands still write to the local store (§1 note) |

## 8. Rotation cadence

- **Calendar-rotate every 90 days**, tied to the token's `Expiration` field:
  mint each new token with a ~90-day expiry so GitHub's own expiry date *is*
  the deadline, and book the rotation in the calendar when you mint. A puller
  credential that lives forever is a credential nobody remembers it exists.
- **Rotate immediately** (same §6 runbook, step 1 = new token, and revoke the
  old one *first* only if it may be live-compromised) on any suspicion: the
  token appeared in a log/screenshot/clipboard mishap, a machine holding it
  is compromised, the owning account's access changed (repo removal — §4 —
  or loss of org membership), or a CI/backup artifact that could embed
  `~/.docker/config.json` left the trusted perimeter (that file contains the
  base64 credential; the join path copies it over ssh, `join.go:430-441`).
- Keep the credential **out of backups in plaintext where avoidable**: the DB
  row is AES-GCM-encrypted, but `~/.pmcluster/.encryption_key` must be backed
  up *with* the DB or every restore lands in the "unrecoverable ciphertext"
  row of §7 (`cipher.go:1-6`).

## 9. The GitHub App alternative

Everything above still describes the fallback. With an App configured
(`pmcluster github app set --app-id … --key-file … --installation-id …`):

| Concern | Stored PAT (`registry add`) | App installation token |
|---|---|---|
| Where the durable secret lives | `registries` row, AES-GCM ciphertext | `github_app` row, AES-GCM ciphertext (the private key) |
| What docker receives | the PAT | a one-hour installation token, user `x-access-token` |
| Delivery to workers | `--with-registry-auth` on deploy | unchanged — same deploy path, same flag |
| Refresh | manual rotation (§6) | daemon re-login every 25 min; token reused for 50 min |
| Precedence for ghcr.io | used when no App is configured, or the App login fails | used when login succeeds; the stored row is skipped |
| Scopes | `read:packages` on the owning account | App permission **Packages: Read**, bounded by the installation |
| Failure signal | deploy error / ImagePullBackOff | `pmcluster doctor` check `github app`, console Test button |

What does **not** change: `pmcluster registry add/list/remove` keep working, a
stored `ghcr.io` row is still replayed on startup when no App is set, and other
registry hosts are never touched by the App path. Removing the App
(`pmcluster github app clear`) makes the next daemon start fall back to the
stored row.
