# GitHub App: installation tokens for pulls and CI

One GitHub App replaces two long-lived credentials that used to live in nine
repositories and one manager:

| Today | Replaced by |
|---|---|
| a personal OAuth token (`GH_PAT`) in every repo's Actions secrets, used through a global `git config insteadOf` rewrite | `POST /api/github/token` — a workflow mints a one-hour installation token per job |
| a classic PAT with `read:packages` stored by `pmcluster registry add ghcr.io` | the same App's installation token, fetched by docker's credential helper at pull time |

The App's private key lives **only** in pmcluster's encrypted control-plane
store. The installation token derived from it is never written to `data.db`,
never rendered into a compose file, and never logged.

Sections: [1 model](#1-the-model-and-its-ceiling) ·
[2 create the app](#2-create-the-app-manually) ·
[3 configure the cluster](#3-configure-the-cluster) ·
[4 the CI broker](#4-the-ci-broker) ·
[5 image pulls](#5-image-pulls) ·
[5.1 the credential helper](#51-the-credential-helper-pull-time-not-a-timer) ·
[6 doctor and the console](#6-doctor-and-the-console) ·
[7 failure modes](#7-failure-modes) ·
[8 rotation](#8-rotation) ·
[9 not built](#9-deliberately-not-built)

---

## 1. The model, and its ceiling

An installation token is issued by GitHub for one installation and carries
**every permission the App was granted**. There is no per-call narrowing: the
token endpoint accepts no permission list, and pmcluster does not pretend it
can add one. The App's own configuration is therefore both the ceiling and the
blast radius. Grant only what is needed:

| Permission | Level | Needed for |
|---|---|---|
| Repository → **Contents** | Read | `actions/checkout`, `go mod download` against private modules |
| Repository → **Metadata** | Read | always-on once any repository permission is granted |
| Repository → **Packages** | Read | pulling private images from GHCR |

Nothing needs write. The App must be installed on the org with access to the
repositories that publish/consume the images and the private Go modules.

Two credentials exist, with different jobs:

- **App private key** (`github_app` table, AES-256-GCM ciphertext). Signs a
  9-minute RS256 JWT, which only GitHub's `/app` surface accepts. It never
  leaves the control plane.
- **Per-repo broker secret** (`github_broker_secrets`, one row per repo). An
  HMAC-SHA256 key a workflow holds to call `POST /api/github/token`. It can
  only mint; it cannot deploy, read secrets, or mint for another repo.

The broker deliberately does **not** accept the daemon bearer token. A bearer
token is full control-plane access; putting one in every repo would recreate
the problem this feature removes, with a larger blast radius.

## 2. Create the app manually

pmcluster does not create the App. Do this once, in GitHub:

1. **Settings → Developer settings → GitHub Apps → New GitHub App.**
2. Homepage URL: anything real (the cluster domain is fine). **Uncheck Webhook
   → Active** — pmcluster's webhook receiver is for deploy hooks, not App
   events, and an active webhook with no URL just generates failures.
3. Repository permissions: **Contents: Read**, **Packages: Read**,
   **Metadata: Read** (GitHub selects Metadata automatically).
4. Where can this app be installed: **Only on this account** (the org).
5. Create, note the **App ID**, then **Generate a private key** — downloads a
   `.pem` once. GitHub cannot show it again.
6. **Install App** on the org, choose the repositories (or all). The
   installation id is in that installation's settings URL —
   `github.com/organizations/<org>/settings/installations/<id>`.

## 3. Configure the cluster

Run on the leader (the `github` command group is leader-scoped, like
`registry`):

```bash
# 1. store the app id and key (encrypted); --installation-id is optional
pmcluster github app set \
  --app-id 123456 \
  --key-file ~/pmcluster-app.pem \
  --installation-id 789 \
  --probe-repo acme/private-modules \
  --probe-package acme/app:latest

# 2. confirm what was stored — the key is never printed
pmcluster github app show

# 3. prove it end to end (a live GitHub call; no test does this for you)
pmcluster github app test
```

- `--key-file` is required the first time. Re-running without it keeps the
  stored key and changes only the ids/probe targets.
- `--probe-repo` and `--probe-package` are what `doctor` and the console's Test
  button read. Without them the only provable claim is "a token was minted",
  which is not "read access works" — doctor says so as a warning.
- `--installation-id` may be omitted; `pmcluster github app show` then prints 0
  and the check reports that no installation is configured.

Clear the registration (broker secrets are kept — they are per-repo revocations,
not app state):

```bash
pmcluster github app clear
```

## 4. The CI broker

### 4.1 Create one secret per repo

```bash
pmcluster github broker add acme/app --description "deploy workflow"
# prints the secret ONCE; store it as the repo secret PMCLUSTER_BROKER_SECRET

pmcluster github broker list     # never prints the secrets
pmcluster github broker rotate acme/app
pmcluster github broker remove acme/app
```

### 4.2 The request

```bash
BODY=""
TS=$(date +%s)
SIG=$(printf '%s' "$TS$BODY" | openssl dgst -sha256 -hmac "$PMCLUSTER_BROKER_SECRET" -hex | sed 's/^.*= //')

curl -fsS -X POST "$PMCLUSTER_API/api/github/token" \
  -H "X-Pmcluster-Repo: acme/app" \
  -H "X-Pmcluster-Timestamp: $TS" \
  -H "X-Pmcluster-Signature: sha256=$SIG"
```

Response:

```json
{"token": "ghs_…", "expires_at": "2026-11-09T13:04:05Z"}
```

Same HMAC scheme and header names as the deploy webhook receiver: the digest is
over `timestamp + body`, the timestamp must be within five minutes, and every
auth failure is one 401 so a caller cannot distinguish "unknown repo" from "bad
signature". Mints are rate-limited per repo (5 immediately, then ~10/minute);
429 carries `Retry-After`.

### 4.3 Workflow snippet

```yaml
name: ci
on: [push]

jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - name: Mint a GitHub App token from pmcluster
        id: pmcluster
        env:
          PMCLUSTER_API: ${{ vars.PMCLUSTER_API }}          # e.g. https://pmcluster.example.com
          PMCLUSTER_BROKER_SECRET: ${{ secrets.PMCLUSTER_BROKER_SECRET }}
        run: |
          set -euo pipefail
          ts=$(date +%s)
          sig=$(printf '%s' "$ts" | openssl dgst -sha256 -hmac "$PMCLUSTER_BROKER_SECRET" -hex | sed 's/^.*= //')
          token=$(curl -fsS -X POST "$PMCLUSTER_API/api/github/token" \
            -H "X-Pmcluster-Repo: $GITHUB_REPOSITORY" \
            -H "X-Pmcluster-Timestamp: $ts" \
            -H "X-Pmcluster-Signature: sha256=$sig" | jq -r .token)
          echo "::add-mask::$token"
          echo "token=$token" >> "$GITHUB_OUTPUT"

      - uses: actions/checkout@v4
        with:
          token: ${{ steps.pmcluster.outputs.token }}

      - name: Private modules and private images
        env:
          GH_TOKEN: ${{ steps.pmcluster.outputs.token }}
        run: |
          set -euo pipefail
          # Replaces the global GH_PAT insteadOf rewrite: the token is scoped to
          # this job and expires in an hour.
          git config --global url."https://x-access-token:${GH_TOKEN}@github.com/".insteadOf "https://github.com/"
          go mod download
          echo "$GH_TOKEN" | docker login ghcr.io -u x-access-token --password-stdin
          docker pull ghcr.io/acme/app:latest
```

Notes that matter:

- The token is masked in the log (`::add-mask::`) and dies with the job. Do not
  write it to a file that outlives the step, and do not pass it to a `uses:`
  action you would not give repo read access anyway.
- `permissions: contents: read` on the job controls the **workflow's own**
  `GITHUB_TOKEN`; the minted App token is independent and is what carries
  cross-repo access.
- `jq` ships on GitHub-hosted runners. On a self-hosted runner, parse the JSON
  with whatever is installed; the contract is only the two response keys.

## 5. Image pulls

A ghcr.io pull authenticates with an installation token, user
`x-access-token`. The token is fetched **when the pull happens**, by a docker
credential helper, from a listener the cluster publishes on every node. Nothing
durable is written: no `docker login`, no credential in
`~/.docker/config.json`, no 25-minute refresh loop, and therefore nothing that
can be stale.

### 5.1 The credential helper: pull-time, not a timer

An installation token lives about an hour. A design that logs in on a timer
holds a credential in docker's config that expires between ticks — after a
manager restart, a paused daemon, or an hour with no tick at all — and the
symptom is a `401` that reads like a missing image. So the credential is
requested per pull instead:

```
dockerd/CLI ──► docker-credential-pmcluster (host, /usr/local/bin)
                    │  presents /var/lib/docker/swarm/certificates/swarm-node.crt
                    ▼
          127.0.0.1:9443  (ingress-published on EVERY node → the edge task)
                    │  TLS + client certificate verified against the swarm root CA
                    ▼
          pmcluster-edge  POST /cred/v1/token  {"server":"ghcr.io"}
                    │  Bearer (the console's own token)
                    ▼
          leader's daemon  POST /api/github/pull-credential
                    │  signs the App JWT, mints the installation token
                    ▼
          {username: "x-access-token", secret: <token>, expires_at: …}
```

The pieces, and why each is where it is:

| Piece | Where | Why |
|---|---|---|
| `docker-credential-pmcluster` | a symlink at `/usr/local/bin` to the `pmcluster` binary (`cluster up`/`cluster update` install it; each daemon re-installs its own at startup) | docker invokes a helper by name (`docker-credential-<suffix>`); the same binary is the CLI, the daemon and the helper, dispatched on `argv[0]` |
| `credHelpers["ghcr.io"] = "pmcluster"` | `/root/.docker/config.json`, **merged** | docker only consults a helper for a host named there. The merge preserves every other key — other registries' `auths`, another `credsStore`, proxies — and is a no-op when the entry is already correct |
| the listener | `pmcluster-edge`, port 9443, `mode: ingress` | the routing mesh answers on `127.0.0.1:9443` on every node and forwards to the one task that can mint, so a node's helper needs no node IP, no socket and no daemon proxy. Same availability shape as the collector's 4318 |
| the minter call | the daemon, `POST /api/github/pull-credential` | the App's private key lives in the leader's store; only the daemon can sign the JWT |

**The certificate rule.** The listener accepts a client only when its TLS
certificate is signed by this swarm's root CA **and**

- `O` equals this swarm's ID (`GET /api/cluster/info` → `swarm.id`), and
- `CN` (the node ID — not the hostname) is a current member of the swarm's
  node list.

Anything else — no certificate, a foreign CA, another swarm's certificate, a
node that left, an expired certificate — is refused before the handler runs.
There is no shared secret: swarm already issued every node a per-node identity,
and a second credential would be one more thing to distribute and rotate. The
rule lives in `pmcluster/internal/pullcred/verify.go`.

**Availability.** The listener runs on the edge, which is pinned to the leader.
A pull therefore requires the leader (and its daemon) to be reachable — the same
dependency the OTLP collector already imposes. A node that cannot reach it fails
the pull with the connection error on stderr rather than hanging.

**What is left in docker's config.** Nothing for ghcr.io: `store` and `erase`
are deliberate no-ops, so `docker login ghcr.io` on a node has nothing to keep.
Every other registry is untouched.

### 5.2 Verifying it

```sh
# the whole chain in one command, exactly as docker runs it
printf 'ghcr.io\n' | /usr/local/bin/docker-credential-pmcluster get
# → {"ServerURL":"ghcr.io","Username":"x-access-token","Secret":"…"}   (exit 0)

# or, wrapped with the diagnosis
pmcluster doctor      # the `github app` line now ends with
                      # "docker-credential-pmcluster get ghcr.io returned a token"
```

`pmcluster doctor` runs the helper, so it proves helper → certificate →
listener → daemon → App → GitHub. The mint alone (the console's Test button)
proves only the key.

Precedence for ghcr.io is now: the helper (App), then a stored
`pmcluster registry add ghcr.io` credential as the fallback when no App is
configured — the same rule as before, with the App no longer replayed by
`docker login`.

## 6. Doctor and the console

`pmcluster doctor` adds one check, `github app`:

| Outcome | Status |
|---|---|
| no app configured | **skip** — a cluster on public images is fine |
| token minted, but no `--probe-repo`/`--probe-package` | **warn** — nothing proved read access |
| key/id rejected by GitHub | **fail** — "the app key/id was rejected" |
| installation gone (uninstalled, wrong app) | **fail** — "the installation is gone or not visible" |
| `contents:read` or `packages:read` missing | **fail** — names the permission and how to grant it |
| token minted, both probes pass, and the node's helper returns a token | **ok** |
| token minted but `docker-credential-pmcluster get ghcr.io` fails | **fail** — names the helper and `pmcluster-edge` |

The console's Settings page shows the app id, installation id, probe targets,
and the granted permission map, with a warning when either required permission
is missing, plus a **Test** button that runs the same probe and reports the
sentence. It never displays the key or a token.

## 7. Failure modes

| Symptom | Cause | Fix |
|---|---|---|
| `401 unauthorized` from the broker | signature does not cover the exact body + timestamp, clock skew > 5 min, or the repo has no secret | recompute with the receiver's rule (`timestamp` then body, `sha256=` prefix); check runner clock; `pmcluster github broker list` |
| `502` with "installation is gone" | app uninstalled, or `--installation-id` belongs to another app | reinstall, then `pmcluster github app set --installation-id …` |
| `502` with "key was rejected" | wrong `.pem` for the App id, or the key was regenerated in GitHub | download the current key, `pmcluster github app set --key-file …` |
| `502` with "lacks a required permission" | App permissions edited but not approved on the installation | GitHub → Installations → the app → approve the new permissions |
| a pull fails with `unauthorized` while `pmcluster github app test` passes | the pull path (helper → edge → daemon), not the key | `pmcluster doctor`; `docker-credential-pmcluster get ghcr.io` and read its stderr |
| a pull fails with `connection refused` on 9443 | the edge is not running on the leader, or the port is not published | `docker service ps pmcluster_edge_pmcluster-edge`; the edge logs "credential listener disabled" with the reason |
| a pull fails with `certificate signed by unknown authority` / `bad certificate` | the node's certificate does not chain to the swarm CA, or the listener's CA mount is missing | `openssl x509 -in /var/lib/docker/swarm/certificates/swarm-node.crt -noout -subject -issuer`; the subject must show this swarm's ID and node ID |
| `docker-credential-pmcluster: not found` on a node | the helper was never installed there | run `pmcluster cluster update` on that node (or restart its daemon, which installs it) |
| a pull fails an hour after a manager restart, broker still works | the old timer design; it no longer exists | if you still see this, the node's `config.json` was edited by hand — re-run `cluster update` |
| `image pulls work, go mod download gets 403` | token minted but the App lacks Contents: Read, or the installation does not include that repo | App permission + installation repository selection; `--probe-repo` catches it in doctor |

## 8. Rotation

- **The installation token rotates itself** every hour; there is nothing to
  calendar. That is the point of the design.
- **Broker secrets** are the durable CI credential. Rotate on suspicion or on
  a schedule with `pmcluster github broker rotate <owner/repo>`; the old secret
  stops working immediately and the workflow keeps its repo-secret name.
- **The App private key** rotates rarely: generate a new key in GitHub, run
  `pmcluster github app set --app-id … --key-file new.pem`, verify with
  `pmcluster github app test`, then delete the old key in GitHub.
- **Existing PATs** can be deleted once every repo's workflow uses the broker
  and `pmcluster github app test` passes. Removing the App (`clear` or
  uninstall) revokes every minted token's future immediately; tokens already
  issued live out their hour.

## 9. Deliberately not built

- **App Manifest flow** (creating/installing the App from the console). The
  operator creates the App by hand. If this becomes necessary the endpoints
  would be: `POST /api/github/app/manifest` (create from a manifest, one-time
  code), `GET /api/github/app/manifest/callback` (exchange the code, store
  id/key), `POST /api/github/app/install` (redirect to the installation URL).
  It needs a public redirect URL and app-owner rights the cluster may not have.
- **A token-display UI.** The broker response is the only place a token is
  shown, to the caller that asked for it.
- **Per-call permission narrowing.** GitHub cannot do it; documenting a fake
  would be worse than documenting the ceiling.
