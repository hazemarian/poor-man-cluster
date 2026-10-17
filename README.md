<p align="center">
  <img src="docs/assets/logo-lockup.svg" alt="Poor Man's Cluster" width="680">
</p>

<p align="center"><strong>SIMPLE • FLEXIBLE • POWERFUL</strong></p>

<p align="center">
  <a href="https://github.com/hazemarian/poor-man-cluster/releases/latest"><img src="https://img.shields.io/github/v/release/hazemarian/poor-man-cluster?color=FF5722&label=release" alt="Latest release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/hazemarian/poor-man-cluster" alt="License: MIT"></a>
  <a href="https://github.com/hazemarian/poor-man-cluster/actions/workflows/pmcluster.yml"><img src="https://github.com/hazemarian/poor-man-cluster/actions/workflows/pmcluster.yml/badge.svg" alt="build"></a>
  <a href="https://github.com/hazemarian/poor-man-cluster/actions/workflows/swarm-e2e.yml"><img src="https://github.com/hazemarian/poor-man-cluster/actions/workflows/swarm-e2e.yml/badge.svg" alt="swarm end-to-end"></a>
  <a href="https://github.com/hazemarian/poor-man-cluster/actions/workflows/docs.yml"><img src="https://github.com/hazemarian/poor-man-cluster/actions/workflows/docs.yml/badge.svg" alt="docs vs code"></a>
  <a href="docs/whitepaper.md"><img src="https://img.shields.io/badge/docs-whitepaper-FF5722" alt="Technical whitepaper"></a>
  <img src="https://img.shields.io/badge/orchestrator-Docker%20Swarm-2496ED?logo=docker&logoColor=white" alt="Docker Swarm">
</p>

# Poor Man's Cluster

A simple, cost-effective, production-ready deployment stack on open-source tools. No Kubernetes, no managed cloud services, no expensive licensing — just Docker Swarm, a small Go control plane (`pmcluster`), and a handful of well-chosen tools that get the job done.

One static Go binary brings a cluster up on cheap VPSes and gives you HTTPS ingress, observability, programmatic deployments, automated backups, and a single CLI (+ web console) to manage it all.

- **Design & trade-offs:** [docs/whitepaper.md](docs/whitepaper.md) — the whitepaper: architecture, design rationale, and what was rejected.
- **Full documentation:** [docs/README.md](docs/README.md)
- **Current release:** [v0.2.180](https://github.com/hazemarian/poor-man-cluster/releases) · [CHANGELOG.md](CHANGELOG.md)

## What you get

- **Declarative deploys** — a small YAML DSL translated to Swarm compose; versioned revisions and one-command rollback.
- **Self-healing cluster** — a leader-side control loop reconciles drift (rendered hashes vs the live swarm) on every pass.
- **Encrypted secrets & configs** — AES-256-GCM in SQLite, mirrored to Swarm as content-addressed objects.
- **Backups that work** — hourly snapshots, double-written to an in-cluster store and offsite S3/R2; routed restores.
- **Operator console** — a web UI for stacks, services, webhooks, TLS, users (RBAC), backups, and settings — no Portainer needed.

## How it works

One machine is the **manager** (runs `pmcluster` + the platform services); any number of **workers** join with one command. Traefik fronts HTTPS ingress; OpenObserve + an OTel collector give you logs/metrics/traces; offen agents back everything up hourly.

```
Internet → Traefik (HTTPS, every manager)
             ├─ pmcluster.<domain>  → pmcluster-edge → operator console (/web)
             │                                       → daemon API + webhooks (/api, /webhook)
             ├─ observ.<domain>     → OpenObserve (auto-auth, no login prompt)
             ├─ traefik.<domain>    → Traefik dashboard
             └─ your apps           → DSL-deployed stacks
```

The DB is the source of truth: `cluster update` re-renders platform configs, re-deploys only what changed, and rebuilds anything missing. Architecture and the design rationale: **[docs/whitepaper.md](docs/whitepaper.md)**.

## Minimum hardware

| Node | CPU | RAM | Notes |
|------|-----|-----|-------|
| Manager (leader) | 2 vCPU | 4 GB | Traefik + OpenObserve + OTel + backup agent + edge + pmcluster |
| Additional manager | 1 vCPU | 1 GB | Traefik (global on managers) + OTel + standby daemon |
| Worker | 1 vCPU | 1 GB | OTel + (backup agent + stateful workloads when a storage node) |

## Quickstart

### Prerequisites

- Docker Engine 20.10+ on the manager (`docker --version`).
- Swarm: pmcluster runs `docker swarm init` transparently on a first node; existing swarms are left untouched.
- TLS, pick one: **Let's Encrypt** (point `*.<your-domain>` at the manager, port 80 reachable) or **your own cert + key** (any CA).

### 1. Install pmcluster

```bash
curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-cluster/main/install.sh | bash
```

Picks the right `darwin|linux` × `arm64|amd64` archive, verifies SHA256, installs to `/usr/local/bin/pmcluster` (`PREFIX=…`, `VERSION=…` to pin). With private registry credentials: `| PMCLUSTER_REGISTRY="ghcr.io=user=token" bash`.

Or build from source (Go 1.25+): `cd pmcluster && make build && sudo install -m 0755 bin/pmcluster /usr/local/bin/`

### 2. Bring the cluster up

```bash
pmcluster init    # creates ~/.pmcluster, prints the admin token ONCE
pmcluster setup   # interactive wizard, then cluster up (or update if one exists)
```

Scripted instead of the wizard:

```bash
pmcluster cluster up --domain=example.com --acme-email=ops@example.com --observability-admin-email=ops@example.com
# or with your own cert:
pmcluster cluster up --domain=example.com --observability-admin-email=ops@example.com \
  --cert=/path/to/cert.pem --key=/path/to/key.pem
```

Bootstrap passwords (Traefik gate / OpenObserve / edge / SeaweedFS) are generated randomly and printed **once**; retrieve later with `pmcluster credentials list` / `credentials show <name>`.

Once DNS resolves: `https://traefik.<domain>` (dashboard), `https://observ.<domain>` (observability), `https://pmcluster.<domain>` (console + API). The daemon runs as a systemd service — no manual unit management.

### 3. Deploy your first app

Save as `app.yaml`:

```yaml
app: my-web
env: production
domain: example.com
registry: ghcr.io/acme        # optional image prefix
version: latest

services:
  api:
    image: ${registry}/${app}:${version}
    replicas: 2
    expose: { port: 8080, host: api.${app}.${domain} }
    healthcheck: { type: http, path: /health }

  db:
    image: postgres:14-alpine
    placement: node-01         # stateful → auto-pinned to a storage node; a hostname here overrides
    volumes: [db_data:/var/lib/postgresql/data]
    env: { POSTGRES_DB: myweb, POSTGRES_USER: db }
    secrets: [my_web_db_password]   # better than plain env — see docs/secrets-and-configs.md
    healthcheck: { type: pg_isready }
```

```bash
pmcluster secret create my_web_db_password    # prints the value once
pmcluster deploy ./app.yaml
pmcluster stack list
pmcluster rollback my-web <rev>      # every deploy is a stored revision
```

Stateful rule of thumb: anything with a volume gets `placement: <specific-node>` (or no placement — it's auto-pinned to a storage node). Full field reference: [docs/dsl.md](docs/dsl.md) · storage strategies: [docs/storage-and-databases.md](docs/storage-and-databases.md) · CI integration: [docs/webhook.md](docs/webhook.md).

### 4. Use the console

`https://pmcluster.<your-domain>` — stacks, live service health/logs/exec/terminal, webhooks, API keys, TLS, backups, users, settings, status badges. Full tour: [docs/console.md](docs/console.md).

### 5. Add worker nodes (optional)

On each additional machine (Docker installed):

```bash
pmcluster join --role worker --token <TOKEN> --manager <MANAGER_IP>:2377
```

Get the token on the manager: `docker swarm join-token worker`. Add `--storage-node` to make the joiner a storage node (backup agent + eligible for stateful pinning). Tailnet join: add `--tailscale --tailscale-auth-key tskey-…` ([docs/network-topology.md](docs/network-topology.md)).

Open between manager ↔ workers: `2377/tcp`, `7946/tcp+udp`, `4789/udp`.

## Daily-use cheat sheet

| Task | Command |
|---|---|
| Deploy / update a stack | `pmcluster deploy ./app.yaml [--version <tag>]` |
| List / inspect / roll back | `pmcluster stack list` / `stack show <name>` / `rollback <name> <rev>` |
| Move a stateful stack | `pmcluster stack move <name> --to <node>` |
| Services: health, logs, exec | `pmcluster service list` / `service logs <stack> <svc>` / `service exec <stack> <svc> -- <cmd>` |
| Secrets / configs | `pmcluster secret …` / `pmcluster config …` — [docs/secrets-and-configs.md](docs/secrets-and-configs.md) |
| Cluster settings | `pmcluster cluster settings list` / `set key=value` — [docs/settings.md](docs/settings.md) |
| Certificates | `pmcluster tls site …` / `tls hosts …` — [docs/tls.md](docs/tls.md) |
| Backups | `pmcluster backup create` / `list` / `browse <id>` / `restore <id>` |
| CI webhook | `pmcluster webhook add <name>` — [docs/webhook.md](docs/webhook.md) |
| Audit logs | `pmcluster logs --tail=200 [--follow]` |
| Reconcile drift / rebuild missing | `pmcluster cluster update` |
| Off-node CLI | `pmcluster --api-url https://pmcluster.example.com --api-token pmc_… <cmd>` |

Alerting (service down, error spikes, memory) is configured in OpenObserve under **Alerts → Alert Rules** — email, Slack, or any HTTP endpoint.

## Tear down

```bash
pmcluster cluster down --yes            # remove platform stacks
pmcluster cluster down --yes --purge    # + secrets/configs/networks + local store
                                        #   (writes a restorable purge-backup-<ts>.tar.gz first)
pmcluster cluster reset --restore <archive>   # rebuild the swarm side from that backup
```

## Where to go next

The full doc map lives at **[docs/README.md](docs/README.md)** — the DSL reference, webhook guide, secrets/configs, TLS, settings, console tour, storage & databases, network topology, security posture, test reports (TC1–TC10), and the whitepaper. The design rationale (why, what was rejected) is in **[docs/whitepaper.md](docs/whitepaper.md)**.

## License

MIT. Copyright © 2025 Hazem Arian.

## Contact

Built and maintained by **Hazem Arian** — [hazeem.arian@gmail.com](mailto:hazeem.arian@gmail.com) · [LinkedIn](https://www.linkedin.com/in/hazem-a-467b4183/). Contributions, issues, and feedback welcome.

## Upgrading a server

Updating the pmcluster binary on the nodes is a scripted, verifiable step. There is no
`upgrade` subcommand: `cluster update` reconciles platform *stacks and configs*, and does not replace the
binary on each node. `pmcluster/hack/upgrade-nodes.sh <version>` does that.

```bash
# from a checkout; needs gh + ssh. Followers first, leader last; checksum verified before anything installs.
PMCLUSTER_NODES=217.160.143.24,217.160.143.26,217.160.143.22 \
  pmcluster/hack/upgrade-nodes.sh v0.2.213
```

The script downloads `pmcluster-<version>-linux-amd64.tar.gz` + `SHA256SUMS.txt`, verifies the SHA256, copies
the binary to each node over `ssh … "cat > /tmp/pmcluster.new"`, stages it in `/tmp`, installs it to
`/usr/local/bin/pmcluster`, restarts the unit, and reports the version before/after on every node. Only once
every node agrees does it run `cluster update` on the leader. The previous binary is kept as
`/usr/local/bin/pmcluster.<version>.prev`.

**Environment variables:**

| Var | Default | Meaning |
|---|---|---|
| `PMCLUSTER_NODES` | *(empty)* | Comma-separated node IPs. **Pass this in practice** — see below. Selects *which* nodes to touch; the restart order is still derived from the swarm when a seed answers. |
| `PMCLUSTER_SEED` | *(empty)* | One reachable node to discover the swarm from; used only when `PMCLUSTER_NODES` is empty (and as the ordering seed when it is set). |
| `PMCLUSTER_SSH_KEY` | `~/.ssh/pmcluster_vps4_ed25519` | SSH private key. |
| `PMCLUSTER_SSH_USER` | `root` | SSH user for every node (set `wafaa` for the wafaa node). |

**Ordering:** workers and follower managers are upgraded **before the leader**, which goes last, so the swarm
never loses its only manager mid-upgrade. "Leader last" is a safety property, not a preference: even when you
name the nodes with `PMCLUSTER_NODES`, the script reads each node's role from the swarm and reorders them.
Only if the swarm cannot be read does it fall back to the order you gave (never aborting).

**Failure semantics:** every step that can fail is fatal. If a node's copy or install fails, the script
finishes the loop and exits non-zero — a partial upgrade cannot pass silently. The copy is strictly before
the install, so a copy failure leaves that node byte-for-byte untouched.

**Always pass `PMCLUSTER_NODES`.** Discovery reads each node's `Status.Addr`, which on these clusters is the
swarm-**internal** address (`10.7.224.x`) — unreachable from outside. The script discovers the nodes happily
and then cannot SSH to any of them.

**The two production targets, and what trips you up on each:**

- **Production swarm** — nodes `217.160.143.22` (leader), `.24`, `.26`, and worker `217.160.128.29`; key
  `~/.ssh/pmcluster_vps4_ed25519`; user `root`.
- **wafaa** — single-node manager at `195.62.50.111`, hostname `idlib-wafaa`; key `~/.ssh/wafaa_m_ed25519`;
  user **`wafaa`** (not root); passwordless `sudo -n` required; sshd has the **SFTP subsystem disabled**; login
  shell is **zsh**.

```bash
# wafaa (single node, non-root user)
PMCLUSTER_NODES=195.62.50.111 PMCLUSTER_SSH_USER=wafaa \
  PMCLUSTER_SSH_KEY=~/.ssh/wafaa_m_ed25519 \
  pmcluster/hack/upgrade-nodes.sh v0.2.213
```

Three traps on wafaa, each of which looks like something it isn't:

- **The user is `wafaa`, not `root`, and `idlib-wafaa` is the hostname, not a username.** Sending the correct
  key to the wrong user fails as `Permission denied (publickey)` — identical to a missing or wrong key, so you
  re-check the key path when the real problem is the username.
- **`scp` needs the SFTP subsystem, which is disabled.** A copy over `scp` fails with `scp: Connection
  closed` — the same message an auth refusal or fail2ban ban produces, though it is neither. That is why the
  script copies with `ssh … "cat > /tmp/pmcluster.new"` as the primary path; `scp -O` is only a fallback.
- **zsh does not word-split unquoted variables.** The remote login shell is the user's own — zsh on wafaa —
  and zsh does not split an unquoted parameter. The old `SUDO="sudo -n"; $SUDO install …` indirection tried to
  exec a command named literally `sudo -n` and died with `zsh: command not found: sudo -n`, so the non-root
  install never worked. The script now writes the privileged step once per branch (`if [ "$(id -u)" -eq 0 ];
  then install …; else sudo -n install …; fi`), which cannot depend on any shell's splitting behaviour.

Then reconcile the platform:

```bash
pmcluster cluster update
```

**Two things to know before you run either**, both learned the hard way:

- **Read `cluster update`'s summary literally — it has two axes.** `Stacks re-deployed : [...]` lists stacks
  whose rendered compose moved; `Configs replaced : [...]` lists rendered configs replaced *without* a stack
  redeploy (e.g. the OTel collector config or the observability provisioner). The no-op line
  `(no stacks redeployed and no configs replaced — nothing to redeploy)` prints only when neither axis moved.
  Before v0.2.212 the summary tracked stack redeploys only, so a replaced config was reported as "nothing
  changed"; if you are on an older binary and a config replacement matters, verify the effect rather than the
  summary — `docker service inspect <svc>` and read the mounted config back.
- **Repeated SSH attempts trip fail2ban**, after which the leader refuses connections while the followers
  still answer. Slow down rather than retrying hard, and check followers to confirm the cluster is otherwise
  healthy.
