# GitHub App: installation tokens for clone, build and CI

One GitHub App replaces the personal OAuth token that used to live in nine
repositories. It does **not** replace the pull credential: image pulls keep a
long-lived read-only PAT, for the reason in §5.

| Long-lived credential | Replaced by |
|---|---|
| a personal OAuth token (`GH_PAT`) in every repo's Actions secrets, used through a global `git config insteadOf` rewrite | `POST /api/github/token` — a workflow mints a one-hour installation token per job |
| — | the read-only PAT for image pulls **stays**, registered with `pmcluster registry add ghcr.io` and documented in [`registry-credentials.md`](registry-credentials.md) |

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
| Repository → **Packages** | Read | optional: only the doctor probe's package HEAD uses it; image pulls use the stored PAT (§5) |

Nothing needs write. The App must be installed on the org with access to the
repositories that publish/consume the images and the private Go modules.

Three credentials exist, with different jobs:

- **App private key** (`github_app` table, AES-256-GCM ciphertext). Signs a
  9-minute RS256 JWT, which only GitHub's `/app` surface accepts. It never
  leaves the control plane.
- **Per-repo broker secret** (`github_broker_secrets`, one row per repo). An
  HMAC-SHA256 key a workflow holds to call `POST /api/github/token`. It can
  only mint; it cannot deploy, read secrets, or mint for another repo.
- **Read-only pull PAT** (`registries` row, AES-GCM ciphertext). Not the App's
  job and not minted by it — `--with-registry-auth` freezes a credential into
  each service spec, and a spec outlives an hour (§5).

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

### 3.1 From the console (no SSH)

`Settings → GitHub App → Set or rotate the App key` carries all three values:
**app id**, **installation id** and the **private key**, as either a pasted
`.pem` or a file upload (64 KiB max), plus the probe repo and package. App id,
installation id and both probe fields are prefilled from the stored
registration, so a rotation is one field, not four.

The submit goes to `POST /web/settings/github/app`, which calls
`POST /api/github/app` on the daemon. Both run the **same** validation as the
CLI, in this order:

1. parse the PEM — PKCS#1 or PKCS#8, RSA only (an encrypted key or a certificate
   is refused by name and line);
2. sign an app JWT and exchange it for an installation token for the
   installation id in the request;
3. read the probe repo (`contents:read`) and HEAD the probe package
   (`packages:read`), when configured;
4. only then encrypt and store the key, and record the probe outcome.

Nothing is written before step 3 succeeds, so a typo in the installation id, a
key from another App, or an installation that cannot mint leaves the working
registration untouched — the previous key keeps minting.

**Partial update is the normal case.** An empty key field keeps the stored key;
an omitted `app_id`/`installation_id` keeps the stored one; omitted probe fields
keep the stored targets. Changing only the installation id still mints for the
*new* id before storing it. Asking for a registration with no key while no key
is stored is a 400 that names the missing key.

The page shows, before and after the save: app id, installation id, **key
fingerprint** (`SHA256:ab:cd:…`, SHA-256 over the public half — stable for one
key, different for another), the last probe result and how long ago it ran. The
submit label reads **Rotate key** when an App already exists and **Save** when it
does not, and a rotation states that **the previous key stops working
immediately**.

**Transport guard.** A private key is never accepted over cleartext: the daemon
refuses the request (400, `code: insecure_transport`) unless it arrived on TLS,
carried `X-Forwarded-Proto: https` (what the edge sets), or came from loopback.
The console applies the same rule to the browser hop first, because its own call
to the daemon is loopback HTTP — it forwards the browser's scheme in
`X-Forwarded-Proto`, so the daemon can tell "TLS at the edge" from "plain HTTP to
the leader".

**Nothing logs the key.** No middleware in the daemon request path buffers or
prints request bodies (the router adds request-id, status-span, rate-limit,
recoverer and timeout middleware only; the `otelhttp` wrapper records method,
route and status). The set route logs one audit line per attempt with the ids,
the fingerprint and the probe outcome, and refusals log a sanitised error — a
PEM header and everything after it is cut before anything is written.

The CLI equivalent, including the `pmcluster setup` flags:

```bash
pmcluster setup --domain example.com --acme-email me@example.com \
  --github-app-id 123456 --github-app-key-file ~/pmcluster-app.pem \
  --github-installation-id 789
```

The three App flags are optional; without them setup prints that the App can be
configured later from the console.

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

**Pulls do not use the App.** The pull credential is the stored read-only PAT
from `pmcluster registry add ghcr.io` ([`registry-credentials.md`](registry-credentials.md)).
The daemon applies it at startup and again before every deploy — one login per
deploy, serialised by a mutex, password on stdin, `DOCKER_CONFIG=/root/.docker`
— and `docker stack deploy --with-registry-auth` forwards it to workers. That
PAT is the only credential `--with-registry-auth` freezes into a service spec.

Why not an installation token: a service spec is immutable after deploy, and
the credential embedded in it is a snapshot. An installation token dies after
an hour, so a task rescheduled onto a node that lacks the image an hour later
fails to pull. A read-only PAT lives 90 days, so the same frozen copy keeps
working for the life of the deploy. That is why the concerns are split:

| Concern | Credential |
|---|---|
| clone private repos, `go mod download`, image builds | GitHub App installation token |
| GitHub Actions token broker (`POST /api/github/token`) | GitHub App installation token |
| `docker pull` and `docker stack deploy --with-registry-auth` | stored read-only PAT |

Every deploy login is announced at WARN, naming the registry, the credential's
source and its age:

```
WRN docker logged in to ghcr.io using the stored registry credential (added 12 days ago); read-only PATs expire at 90 days — rotate with `pmcluster registry add ghcr.io`
```

That line is the operator's early warning. `pmcluster doctor` carries the same
threshold independently: its `registry credential` check warns once the stored
credential is 60 days old, which leaves a month before the 90-day PAT expiry.

A failed login warns with docker's own stderr (`unauthorized`, `denied:
denied`, or a daemon-connection error) and does **not** abort the deploy.
Docker's pull error names the real problem, and doctor is the intended
detector.

**Residual risk, stated plainly.** If the PAT is revoked or reaches its 90-day
expiry, new pulls fail. Nothing renews it on a timer — that was the point of
removing the 25-minute App-token loop. Rotation is an operator action
(`pmcluster registry add ghcr.io`, runbook in `registry-credentials.md` §6),
and `pmcluster doctor` warns from day 60 so it is not discovered by a failed
deploy. A task rescheduled after a mid-deploy revocation can still fail to
pull; **pre-pull** and an **in-cluster registry mirror** would cover that and
are not implemented.

Nothing about the PAT is rendered into a compose file by pmcluster. `docker
login` writes it to Docker's own `~/.docker/config.json`, the file
`--with-registry-auth` already reads; `data.db` holds only its ciphertext.

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
the key's fingerprint, the last probe result and its age, and the granted
permission map, with a warning when either required permission is missing, plus a
**Test** button that runs the same probe and reports the sentence. It never
displays the key or a token. The same panel carries the set/rotate form (§3.1);
the submit is operator-only and refused over cleartext.

## 7. Failure modes

| Symptom | Cause | Fix |
|---|---|---|
| `401 unauthorized` from the broker | signature does not cover the exact body + timestamp, clock skew > 5 min, or the repo has no secret | recompute with the receiver's rule (`timestamp` then body, `sha256=` prefix); check runner clock; `pmcluster github broker list` |
| `502` with "installation is gone" | app uninstalled, or `--installation-id` belongs to another app | reinstall, then `pmcluster github app set --installation-id …` |
| `502` with "key was rejected" | wrong `.pem` for the App id, or the key was regenerated in GitHub | download the current key, `pmcluster github app set --key-file …` |
| `502` with "lacks a required permission" | App permissions edited but not approved on the installation | GitHub → Installations → the app → approve the new permissions |
| pulls fail with `denied`/`unauthorized`, broker and builds still work | the stored read-only PAT was revoked or hit its 90-day expiry (pulls never use the App, §5) | `pmcluster doctor` → `registry credential`; rotate with `pmcluster registry add ghcr.io`, then re-deploy |
| pulls fail only on a node that never had the image | that node never received the forwarded credential (an old spec, or a deploy that ran before the PAT existed) | re-deploy the stack so `--with-registry-auth` forwards the current PAT; `pmcluster doctor` rules out an expired credential |
| `image pulls work, go mod download gets 403` | token minted but the App lacks Contents: Read, or the installation does not include that repo | App permission + installation repository selection; `--probe-repo` catches it in doctor |

## 8. Rotation

- **The installation token rotates itself** every hour; there is nothing to
  calendar. That is the point of the design.
- **Broker secrets** are the durable CI credential. Rotate on suspicion or on
  a schedule with `pmcluster github broker rotate <owner/repo>`; the old secret
  stops working immediately and the workflow keeps its repo-secret name.
- **The App private key** rotates rarely: generate a new key in GitHub, then
  either use **Settings → GitHub App → Set or rotate the App key** in the console
  (paste or upload the new `.pem`, leave the ids prefilled) or run
  `pmcluster github app set --app-id … --key-file new.pem`, verify with
  `pmcluster github app test`, then delete the old key in GitHub. The console
  path mints a token with the new key before storing it, so a wrong file changes
  nothing; on success the previous key stops working immediately. The panel shows
  the fingerprint before and after, which is how you confirm the rotation landed.
- **The Actions PATs** can be deleted once every repo's workflow uses the
  broker and `pmcluster github app test` passes. The **read-only pull PAT
  stays** — it is the image-pull credential (§5), rotated on its own 90-day
  calendar (`registry-credentials.md` §6/§8). Removing the App (`clear` or
  uninstall) revokes every minted token's future immediately; tokens already
  issued live out their hour, and pulls keep working because they never
  depended on the App.

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
- **Pre-pull, and an in-cluster registry mirror.** Neither is implemented: no
  `cluster up`/`update` pre-pull step, no mirror or pull-through cache. They
  would cover the one remaining pull gap — a task rescheduled after the stored
  PAT was revoked or expired (§5). A pull helper on every node was tried and
  removed: it solved manual pulls only, cost a helper binary, a second listener
  and mTLS, and is not needed now that the long-lived PAT is what specs carry.
