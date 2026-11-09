# GitHub App: installation tokens for pulls and CI

One GitHub App replaces two long-lived credentials that used to live in nine
repositories and one manager:

| Today | Replaced by |
|---|---|
| a personal OAuth token (`GH_PAT`) in every repo's Actions secrets, used through a global `git config insteadOf` rewrite | `POST /api/github/token` — a workflow mints a one-hour installation token per job |
| a classic PAT with `read:packages` stored by `pmcluster registry add ghcr.io` | the same App's installation token is the GHCR password at deploy time |

The App's private key lives **only** in pmcluster's encrypted control-plane
store. The installation token derived from it is never written to `data.db`,
never rendered into a compose file, and never logged.

Sections: [1 model](#1-the-model-and-its-ceiling) ·
[2 create the app](#2-create-the-app-manually) ·
[3 configure the cluster](#3-configure-the-cluster) ·
[4 the CI broker](#4-the-ci-broker) ·
[5 image pulls](#5-image-pulls) ·
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

When an App is configured, the node running `pmcluster serve` mints an
installation token and logs in to GHCR at startup, **every 25 minutes**, and
**once immediately on gaining swarm leadership**:

- user: `x-access-token`
- password: the minted installation token (stdin, never argv)

Twenty-five minutes is two logins per token lifetime: a token minted at t=0
expires at t=60, so the next login at t=25 and the one at t=50 are both inside
its life, with ten minutes of headroom at every tick. The leadership-gain login
exists because a new leader would otherwise deploy with whatever credential
happened to be on disk — possibly one minted by the previous leader an hour
ago — and freeze that dead token into the specs the first reconcile writes. A
failed login on leadership gain is a warning, not a blocker: the reconcile
proceeds and the timer retries within 25 minutes.

The loop is a plain goroutine inside `pmcluster serve` — no credential helper,
no second listener, no extra port, and no certificate verification on this
path. Every login is serialised by one mutex, so a tick and a failover can
never write docker's config at the same time, and `DOCKER_CONFIG=/root/.docker`
is set explicitly because under systemd `HOME` may not point where docker looks.

Every successful login emits a **warning**, in the daemon journal and in the
deploy output:

```
WRN docker logged in to ghcr.io with a GitHub App token; it expires at
2026-11-09T19:40:12Z and is renewed every 25 minutes
```

A failed login warns with docker's own stderr (`unauthorized`, `denied:
denied`, or a daemon-connection error) and does not abort anything.

`docker stack deploy --with-registry-auth` forwards that login to workers:
every service spec the deploy creates carries the `x-access-token` credential
in its registry-auth object, exactly as it did for the stored registry
credential. That is what lets a node join the service and pull the image.

**The frozen-copy limitation, stated plainly.** A service spec is immutable
after deploy. The credential `--with-registry-auth` embeds in it is a snapshot
taken at deploy time, and it is dead 60 minutes later. The timer keeps the
*node's* docker config fresh and keeps *future* deploys fresh; it does not and
cannot rewrite a spec that is already running. So:

- a task rescheduled onto a node that does not already have the image, more
  than an hour after the deploy, can still fail to pull it
  (`ImagePullBackOff`);
- pulling that image by hand on the node works, because the timer refreshed
  the node's `~/.docker/config.json` — that is the manual workaround;
- re-deploying the stack re-stamps a fresh credential into the spec.

The acknowledged fixes are **pre-pull** (fetch the image on every node before
a task is scheduled there) and an **in-cluster registry mirror** (pull through
a registry that holds the image and carries its own credential). Neither is
implemented: `cluster up`/`update` do not pre-pull, and there is no mirror.
This is a known gap in the design, not a supported configuration.

Precedence is deliberate:

1. App configured and its login succeeds → the stored `ghcr.io` row from
   `pmcluster registry add` is **skipped** for that daemon run, so docker's
   config holds the fresher credential.
2. No App, or the App login fails → the stored registry credential is replayed
   unchanged. The existing path is never broken by a missing/broken App.
3. Other registry hosts are untouched in both cases.

Nothing about the token is persisted by pmcluster. `docker login` writes it to
Docker's own `~/.docker/config.json`, which is the mechanism
`--with-registry-auth` already depends on; `data.db` holds only the encrypted
App key.

## 6. Doctor and the console

`pmcluster doctor` adds one check, `github app`:

| Outcome | Status |
|---|---|
| no app configured | **skip** — a cluster on public images is fine |
| token minted, but no `--probe-repo`/`--probe-package` | **warn** — nothing proved read access |
| key/id rejected by GitHub | **fail** — "the app key/id was rejected" |
| installation gone (uninstalled, wrong app) | **fail** — "the installation is gone or not visible" |
| `contents:read` or `packages:read` missing | **fail** — names the permission and how to grant it |
| token minted and both probes pass | **ok** |

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
| pulls fail an hour after a **deploy**, broker still works | the service spec holds the credential frozen at deploy time, and an installation token dies after an hour; a task rescheduled onto a node without the image cannot pull (§5) | re-deploy the stack (re-stamps the spec), or pull the image by hand on that node — the daemon's 25-minute login keeps the node's docker config fresh |
| pulls fail on a node right after a deploy, broker still works | the App login itself is failing on that node, so `--with-registry-auth` forwarded a stale or missing credential | daemon log: "GitHub App registry login had issues"; run `pmcluster github app test` |
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
- **Pre-pull, and an in-cluster registry mirror.** Both are the acknowledged
  fixes for the frozen-credential gap in §5 (a spec's credential is dead one
  hour after deploy). Neither is implemented: no `cluster up`/`update` pre-pull
  step, no mirror or pull-through cache. A pull helper on every node was tried
  and removed — it solved manual pulls only, left the frozen spec untouched,
  and cost a helper binary, a second listener and mTLS on the pull path.
