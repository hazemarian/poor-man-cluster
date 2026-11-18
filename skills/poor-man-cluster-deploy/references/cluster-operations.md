<!-- moved out of the SKILL.md body on the 2026-11 docs pass; content verbatim -->

## Pre-Deploy Backups

Add `backup_before_deploy: true` to a manifest to take a snapshot backup before deployment. By default the deploy proceeds even if the backup fails (best-effort semantics).

To abort the deploy on backup failure, also set `strict_backup: true`:

```yaml
backup_before_deploy: true
strict_backup: true   # abort the deploy if the backup fails
```

The daemon flushes the SQLite WAL before triggering each backup so the snapshot captures a consistent database state.

```bash
pmcluster backup list                      # audit log of every triggered run
pmcluster backup create                    # on-demand snapshot
```

### Control-plane backups (pmcluster's own state)

The backup stack ships the in-cluster store and nothing else. The control-plane archiver (`control-plane-backup`, `pmcluster-ctlplane-` prefix) is **deleted** (ADR 0005): cluster state reaches every manager by the mTLS mirror, so there is no control-plane backup to take. **Backups are service volumes only.** `pmcluster cluster mirror verify` is the drill that replaced `cluster kit verify`.

This closes the "lost manager disk = full re-bootstrap" gap: app volumes AND the control plane are both archived daily. The `backup` stack is rendered with the daemon's data dir (`${DATA_DIR}`), so the bind mount points at the real `~/.pmcluster` on the manager.


## Cluster Management

```bash
pmcluster cluster status                   # health overview
pmcluster cluster update                   # content-aware reconcile (DB is source of truth)
pmcluster cluster down --yes               # remove all stacks (infra, edge, observability, backup, sso)
pmcluster cluster down --yes --purge       # also remove secrets, configs, networks
pmcluster node list                        # Swarm nodes
pmcluster node join-token worker           # get join token for new workers
```

`cluster up` is init-only — it refuses to run on an already-initialised cluster (run `cluster update` instead). `cluster update` is the content-aware reconcile: the DB is the source of truth for all platform configs (there are no `~/.pmcluster/config/*.yml` disk files). It compares `rendered_hash` to decide which platform stacks (observability / infra / edge / backup / +sso) need re-deploying. Drift-prune removes services that were dropped from a compose. It is idempotent.

The summary reports **stack redeploys and config replacements on two separate axes**, and must be read literally:

- `Stacks re-deployed     : [...]` — stacks whose rendered compose moved (or that drifted out of band in the live swarm).
- `Configs replaced       : [...]` — rendered configs replaced **without** a stack redeploy (e.g. the OTel collector config or the observability provisioner), printed only when non-empty.

The no-op line is printed only when **neither** axis moved:

```
  (no stacks redeployed and no configs replaced — nothing to redeploy)
```

A TLS cert/key rotation with no stack or config change is called out separately rather than folded into that sentence (`… only the TLS cert/key secret rotated`). Before v0.2.212 the summary tracked stack redeploys only, so a replaced rendered config was reported as "nothing changed" while the OTel config and the provisioner were in fact swapped. On an older binary, verify a suspected config replacement with `docker service inspect <svc>` and read the mounted config back instead of trusting the summary.

### Upgrading pmcluster / the edge service

Two binaries are involved: the `pmcluster` binary on each node (the control plane) and the version-pinned `pmcluster-edge` image the platform deploys. `cluster update` reconciles the second — it does **not** replace the binary on the nodes, and there is no `upgrade` subcommand.

#### 1. Build + publish the release first

Push a `v*` tag; the release workflow cross-compiles the binaries and pushes the new edge image to GHCR:

```bash
git tag v0.2.213 && git push origin v0.2.213
```

#### 2. Replace the binary on every node — `hack/upgrade-nodes.sh`

```bash
pmcluster/hack/upgrade-nodes.sh <version>
```

Run from a checkout (needs `gh` and `ssh`). It downloads `pmcluster-<version>-linux-amd64.tar.gz` + `SHA256SUMS.txt`, **verifies the checksum**, copies the binary to each node, stages it in `/tmp`, installs it to `/usr/local/bin/pmcluster`, restarts the unit, and prints the version before/after per node. Only once every node agrees does it run `cluster update` on the leader, so the re-render is done by the new version everywhere. The previous binary is kept as `/usr/local/bin/pmcluster.<version>.prev`.

| Env var | Default | Meaning |
|---|---|---|
| `PMCLUSTER_NODES` | *(empty)* | Comma-separated node IPs. **Pass this in practice** — see the discovery caveat below. Selects *which* nodes to touch; the restart order is still derived from the swarm when a seed answers. |
| `PMCLUSTER_SEED` | *(empty)* | One reachable node to discover the swarm from; used only when `PMCLUSTER_NODES` is empty (and as the ordering seed when it is set). |
| `PMCLUSTER_SSH_KEY` | `~/.ssh/pmcluster_vps4_ed25519` | SSH private key. |
| `PMCLUSTER_SSH_USER` | `root` | SSH user for every node. Set to `wafaa` for the wafaa node. |

**Ordering:** workers and follower managers are upgraded **before the leader**, which goes last, so the swarm never loses its only manager mid-upgrade. This is a safety property, not a preference: even with `PMCLUSTER_NODES` set, the script reads each node's role from the swarm and reorders the list — falling back to the stated order (never aborting) only if the swarm cannot be read.

**Failure semantics:** every step that can fail is fatal. If any node's copy or install fails, the script sets `FAILED`, finishes the loop, and exits non-zero (`✖ upgrade incomplete — some nodes are still on the old binary`) — a partial upgrade never passes silently. The copy is deliberately separate from **and strictly before** the install, so a copy failure leaves that node byte-for-byte untouched and prints `copy FAILED … nothing changed on this node`. A half-upgraded cluster is treated as worse than an un-upgraded one.

**The two production targets** (hard-won access facts — do not re-derive them):

- **Production swarm** — nodes `217.160.143.22` (leader), `217.160.143.24`, `217.160.143.26`, and worker `217.160.128.29`; key `~/.ssh/pmcluster_vps4_ed25519`; user `root`.
- **wafaa** — single-node manager at `195.62.50.111`, hostname `idlib-wafaa`; key `~/.ssh/wafaa_m_ed25519`; user **`wafaa`** (not root); **passwordless `sudo -n` required**; sshd has the **SFTP subsystem disabled**; login shell is **zsh**.

```bash
# production swarm (followers-first order; the leader is listed last)
PMCLUSTER_NODES=217.160.143.24,217.160.143.26,217.160.128.29,217.160.143.22 \
  PMCLUSTER_SSH_KEY=~/.ssh/pmcluster_vps4_ed25519 \
  pmcluster/hack/upgrade-nodes.sh v0.2.213

# wafaa (single node, non-root user)
PMCLUSTER_NODES=195.62.50.111 PMCLUSTER_SSH_USER=wafaa \
  PMCLUSTER_SSH_KEY=~/.ssh/wafaa_m_ed25519 \
  pmcluster/hack/upgrade-nodes.sh v0.2.213
```

> **Always pass `PMCLUSTER_NODES`.** Discovery reads each node's `Status.Addr`, which on these clusters is the swarm-**internal** address (`10.7.224.x`) — unreachable from outside. The script discovers them happily and then cannot SSH to a single one (it now prints a warning on stderr saying exactly this). Pass the public IPs explicitly, or set `PMCLUSTER_SEED` to a host from which those internal addresses do resolve.

#### Traps that cost real time — read before debugging a failed upgrade

1. **The user is `wafaa`, not `root`, and `idlib-wafaa` is the hostname, not a username.** wafaa's sshd accepts only the `wafaa` account; it does not accept `root`. Sending the correct key to the wrong user fails as `Permission denied (publickey)` — **indistinguishable from a missing or wrong key**, so the obvious move is to re-check the key path when the real problem is the username.
2. **`scp` needs the SFTP subsystem.** wafaa has it disabled, so a copy over `scp` fails with `scp: Connection closed`. On OpenSSH 9.x that message also looks like an auth refusal or a fail2ban ban — it is neither, and it sends you into `sshd_config` and firewall rules that were never the problem. The script's **primary copy path is `ssh "$host" "cat > /tmp/pmcluster.new" < "$BIN"`** (the exec channel only, so it works wherever `ssh` works); `scp -O` (legacy SCP protocol, also skips SFTP) is a **fallback only**. `scp` is never the primary path.
3. **zsh does not word-split unquoted variables.** wafaa's login shell is zsh, and zsh does not split an unquoted parameter. The `SUDO="sudo -n"; $SUDO install …` indirection therefore tried to exec a command named literally `sudo -n` and died with `zsh: command not found: sudo -n` — so the non-root install and `systemctl restart` never ran. (The same bug sat in the single-quoted `cluster update` one-liner, where the failure was piped into `grep | tail`, exited 0, and silently skipped the reconcile.) Privileged steps are now written out once per branch — `if [ "$(id -u)" -eq 0 ]; then install …; else sudo -n install …; fi` — which cannot depend on any shell's splitting behaviour.

#### 3. Reconcile the platform

```bash
pmcluster cluster update
```

On a **single node**, `install.sh` is the alternative to steps 1–2: it installs the new binary and, because `~/.pmcluster/config.yaml` exists, automatically runs `pmcluster cluster update`:

```bash
curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-cluster/main/install.sh | VERSION=v0.2.213 bash
# or simply: | bash   (resolves latest release)
```

`cluster update` **re-syncs the platform config templates** from the new binary's embedded copies into the store (the DB is the source of truth; operator edits are preserved), then re-renders. Because the `edge-stack.yml` content changed, the **edge stack is re-deployed** automatically and pulls the new version-pinned image. OTel/Traefik/cert are re-applied content-aware as usual. A second `cluster update` with no changes reports `(no stacks redeployed and no configs replaced — nothing to redeploy)`.

Manual `docker service update --image ... edge_pmcluster-edge` is NOT the supported path — always use the tag → binary upgrade (`hack/upgrade-nodes.sh` on a multi-node swarm, `install.sh` on a single node) → `cluster update` flow. On a fresh box, install.sh runs `cluster up` instead (when `PMCLUSTER_DOMAIN` is set). Set `CLUSTER_APPLY=none` to skip the auto apply.


## Upgrading a multi-node deployment (`hack/upgrade-nodes.sh`)

```bash
cd pmcluster
PMCLUSTER_NODES=<ip>,<ip>,<ip> \
PMCLUSTER_SSH_KEY=$HOME/.ssh/<key> \
bash hack/upgrade-nodes.sh v0.2.218
```

The script downloads the release, verifies its checksum, copies the binary over `ssh … "cat > /tmp/x"`
(SFTP is not required), installs it, restarts the unit, and only then runs `pmcluster cluster update`
on the last node in the list.

### Two traps, both silent

**1. A public IP list defeats the role ordering.** The script sorts your list by the swarm's roles, but
it matches entries against `docker node inspect … {{.Status.Addr}}` — the *internal* address
(`10.7.224.x` here). Public IPs match nothing, so every node is treated as a plain worker and your
stated order is used as-is. "Leader last" is then **your** job: put the leader at the end of
`PMCLUSTER_NODES`. (Same caveat as discovery: internal addresses are right for ordering and wrong as
SSH targets.)

**2. A timeout on port 22 is usually a stale address, not a down host.** Before concluding a node is
unreachable, ask the node itself, through one that is reachable:

```bash
# from a reachable node, jump to the suspect node's INTERNAL address
ssh -i $KEY -o "ProxyCommand=ssh -i $KEY -W %h:%p root@<reachable-node>" root@10.7.224.x '
  hostname; ip -4 -o addr show scope global | awk "{print \$2, \$4}"; ss -ltnp | grep ":22"'
```

Full `ping` loss plus a port-22 timeout, while the swarm reports the node `Ready`, means the swarm is
fine and the address you typed is not the node's current public IP (rebuilt hosts get new IPs from the
provider). Re-run with the address the node reports; do not touch its firewall.

### Recovering a node reachable only on the internal network

Mirror the script's own steps over a jump host — never `docker service update` your way around it:

```bash
gh release download <version> -R hazemarian/poor-man-cluster -p '*linux-amd64.tar.gz' -p 'SHA256SUMS.txt'
shasum -a 256 -c want.txt && tar -xzf pmcluster-<version>-linux-amd64.tar.gz
ssh -o "ProxyCommand=ssh -i $KEY -W %h:%p root@<reachable-node>" root@10.7.224.x \
  'cat > /tmp/pmcluster.new' < pmcluster-<version>-linux-amd64
ssh -o "ProxyCommand=…" root@10.7.224.x '
  cp -a /usr/local/bin/pmcluster /usr/local/bin/pmcluster.<old>.prev
  install -m 0755 /tmp/pmcluster.new /usr/local/bin/pmcluster && rm -f /tmp/pmcluster.new
  systemctl restart pmcluster && sleep 6
  pmcluster version | head -1; systemctl is-active pmcluster'
```

Then run `pmcluster cluster update` on the leader once every node reports the new version — a mixed
cluster is worse than an un-upgraded one.
