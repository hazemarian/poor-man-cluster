# Self-hosted GitHub Actions runners

When the org's GitHub-hosted Actions minutes run out, CI stops. This feature runs
**one ephemeral GitHub Actions runner container per private repository** on the
cluster, registered with a repo-scoped registration token minted from the
GitHub App installation. Nothing about it is manual: turning the setting on
registers and runs the runners, turning it off removes the containers **and**
de-registers the runners from GitHub.

It needs the [GitHub App](github-app.md) — see [Why the App, not a
PAT](#why-the-app-not-a-pat) below.

## Settings

| Key | Default | What it does |
|---|---|---|
| `github_runners_enabled` | `false` | master switch. **Off by default**: a runner mounts the host's docker socket, which is root-equivalent, so the platform never turns this on for an operator |
| `github_runner_repos` | unset | comma-separated `owner/name` list; one runner per entry, **private repositories only** |
| `github_runner_memory` | `2560m` | per-container memory limit (a Docker quantity: `2560m`, `3g`). `2560m` is the value that completed a `govulncheck` run on real hardware; at `1500m` the runner was OOM-killed mid-job |
| `github_runner_allow_leader` | `false` | lift the placement constraint that keeps runners off the leader. Only for a single-node cluster |

Enabling the feature without a stored App registration is **refused** by the
settings surface (CLI, REST and console all go through the same validator), with
an error naming the App and why a personal access token is not a substitute.
Disabling it is never gated: an operator must always be able to take the
docker-socket exposure away.

```bash
pmcluster cluster settings set \
  github_runners_enabled=true \
  github_runner_repos=acme/app,acme/api \
  github_runner_memory=2560m
```

The reconcile loop picks the settings up on its next pass (60 s default). No
`cluster update` is needed: a runner is not a platform config.

## Lifecycle

Each configured repo gets its own one-service Swarm stack
(`pmcluster-runner-<owner>-<repo>-<hash>`), so removing one repo removes exactly
one runner and leaves the others running.

1. **Register.** The control plane mints a repo-scoped registration token with
   the installation token:
   `POST /repos/{owner}/{repo}/actions/runners/registration-token`. The token is
   single-use and lives one hour, so it is handed to a container at creation
   time and **never persisted** — not in the store, not in a rendered config.
2. **Run.** The container starts with `EPHEMERAL=true`, labels
   `self-hosted,linux,x64,pmcluster`, the memory limit, and the docker socket
   bound from the host. It takes **one job**, then exits.
3. **Recycle.** The next reconcile sees a stack whose task has exited, mints a
   fresh token and redeploys it. A container that is still starting (0/1 while
   the image pulls) is left alone for a two-minute cooldown, so a pass never
   mints a token every 60 seconds for a runner that has not started yet.
4. **Remove.** Dropping a repo from `github_runner_repos` (or turning
   `github_runners_enabled` off) removes the stack **and** calls
   `DELETE /repos/{owner}/{repo}/actions/runners/{id}` for every runner whose
   name carries the reserved `pmc-` prefix. A failed de-registration keeps the
   booking entry and is retried next pass; a convergence where the container is
   already gone still de-registers from the name in the booking entry.

The rendered compose is deliberately boring, and every choice is a security
decision:

- `placement: node.labels.pmcluster.leader != true` unless
  `github_runner_allow_leader=true` — a runner builds images and competes with
  the control plane. Swarm evaluates `!=` as true for nodes without the label,
  so ordinary workers still match.
- `restart_policy: none` — an ephemeral runner must never be restarted onto a
  spent registration token.
- `update_config: order: stop-first` — two containers must never register the
  same runner name at once.
- no `network_mode: host` and no published ports.

## Why the App, not a PAT

A registration token is scoped to one repository and must be minted with
`administration: write` on it. The App already mints one-hour installation
tokens for clone, build and the [token broker](github-app.md); using the same
installation for runner registration means no long-lived credential exists
anywhere in the feature. A personal access token could do the same job only by
holding `administration: write` on every repository — a wider, longer-lived
credential — so it is refused by name rather than accepted as a quieter
substitute.

The only App permission this feature adds beyond the pull/clone set is
**Repository → Administration: Read & write**. Grant it in the App's settings,
then approve the new permission on the installation; until then registration
returns a bare `403` and `pmcluster doctor` says exactly which permission is
missing.

## Security

A runner mounts `/var/run/docker.sock`, so **any workflow that runs on it can do
anything on that host as root**. That is the reason for the guards, not a
footnote:

- **Private repositories only.** A public repo's fork pull request would run
  attacker-controlled workflow code on the socket. The reconcile asks GitHub
  whether the repo is private and refuses a public one with that reasoning in
  the warning.
- **Labels are explicit.** `runs-on: [self-hosted, pmcluster]` selects these
  runners; a workflow that only says `self-hosted` is not guaranteed to land
  here.
- **Reserved name prefix.** Every runner this platform creates is named
  `pmc-<node>-<repo>-<hash>`, and de-registration matches that prefix. A runner
  of your own named `pmc-*` would be removed too — do not use the prefix.
- **Nothing is on by default.** The feature, the leader override and the repo
  list are all opt-in.

## Failure handling

| What happened | What the platform does |
|---|---|
| registration token refused (`403`) | warning naming `administration:write`; the other repos still converge. Recorded in the last-apply record so `doctor`'s `last apply` check shows it |
| repo is public | warning explaining the fork-PR risk; no runner is registered for it |
| repo not visible to the installation | warning naming the installation's repository selection and the repo |
| stack deploy fails | warning; retried next pass with a fresh token |
| GitHub is unreachable | the container is still removed on teardown; the de-registration failure is reported, not swallowed, and retried |
| runner exists but container is gone | de-registered by name, then re-registered on a later pass |

None of these fail the reconcile pass: one broken repo must not stop the others.

## Doctor

`pmcluster doctor` reports a `github runners` check:

- **disabled** → `ok` (nothing is deployed, nothing to inspect);
- **enabled with no App** → `fail`, naming the App requirement and the command;
- **App missing `administration:write`** → `fail`, naming the exact permission
  and where to grant it;
- **enabled with no repos** → `warn`;
- **per repo** → `warn` when the container is running but GitHub does not list
  the runner, when GitHub lists it but no container runs (a ghost that will
  never pick up a job), when it is offline, or when registration has failed on
  three consecutive passes.

## What is not built

- Runners are repo-scoped only. Organisation-scoped runners (one registration
  serving many repos) are not implemented; the API path is different and the
  blast radius is larger.
- The runner image is `myoung34/github-runner:latest`, a constant rather than a
  setting: an arbitrary image here would run with the host's docker socket.
- A runner cannot be pinned to a specific node — only kept off the leader (or
  allowed on it).
