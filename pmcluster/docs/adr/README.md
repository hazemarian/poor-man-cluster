# Decision records

One file per decision that constrains the code, written when the decision is made
rather than reconstructed later. An ADR is not a design doc: it records the choice,
what was rejected and why, and what the choice costs — the design docs describe
what the system is. A record that has served its purpose (the code is the
decision, the alternatives are no longer live) is deleted rather than kept as
decoration; the design docs carry the behaviour.

| # | Decision | Status | Date | Settles |
|---|---|---|---|---|
| [0001](0001-engine-seam.md) | Keep Swarm behind a translation layer | accepted (layers 1–2 landed) | 2026-10-27 | Placement is expressed in platform terms in `internal/engine`; engine syntax lives only in `internal/engine/<engine>` |
| [0002](0002-telemetry-backend.md) | Telemetry store: owned query API over parquet (DuckDB), not an owned engine | proposed | 2026-11-07 | Every statement is written by `internal/observability`; the store is replaceable and no backend dialect leaks out of its adapter |
| [0003](0003-cluster-state-by-pull.md) | Cluster state reaches other managers by pull (mTLS snapshot), not by Docker config | accepted (slices 1–3 landed) | 2026-11-13 | `internal/clustersnapshot` is the only reader of cluster state over the network; the version is a content identity stamped by the writer; the cipher key never comes from the replicated object |
| [0004](0004-node-runtime-and-host-units.md) | A node's loops and its host units leave `internal/cli` | accepted | 2026-11-15 | `internal/noderuntime` owns the loops `serve` starts, `internal/nodeunits` owns the units and the node-local file names, `store.OpenExisting` is the one never-create opener, and `cli/boundary_test.go` fails when a name is defined twice or a loop returns to the CLI |
| [0005](0005-mirror-is-the-document.md) | The mirror is the whole control-plane document and adoption copies nothing | accepted | 2026-11-15 | The mTLS snapshot is the only channel for cluster state; every table travels except an explicit node-local exclusion list; a stale mirror is served and logged while an empty store refuses leadership; the config channel's data kit and the control-plane backup path are retired |
| [0006](0006-the-key-is-a-file.md) | The encryption key is a file, not a delivery channel | accepted (route not built yet) | 2026-11-15 | `<data dir>/.encryption_key`, minted once and never rotated; nothing publishes it; a node holding `data.db` never mints one (`credentials.OpenExisting`); the key is proven by decrypting stored secrets, and a wrong key is a warning rather than a refusal |
| [0007](0007-manual-platform-export.md) | The control plane's restore point is a manual export that carries the key | accepted | 2026-11-15 | One operator-triggered archive of the leader's `data.db` + `.encryption_key` to the offsite bucket (console button and `backup platform`); leader-only; no schedule, no automatic restore; deliberate departure from ADR 0003's split because an export without its key cannot decrypt what it protects |
| [0008](0008-agent-and-drill.md) | The backup agent is part of being a storage node, and the export is drilled | accepted | 2026-11-16 | Becoming a storage node installs the agent during `cluster update` (injected installer, nil reported); `backup agents` fails on the leader when a storage node goes quiet for 7 days; `backup platform-verify` proves an export restores (manifest, integrity_check, key-versus-ciphertext) |
| [0009](0009-the-shell-is-a-container.md) | The terminal runs in a throwaway shell, not the service's container | accepted | 2026-11-17 | Exec only works where the container is; the console's daemon is the leader. A shell service pinned to that node, joined to the stack's networks with all its secrets and configs, exec'd over a ticket-bound socket and destroyed with it (30-minute ceiling) |
| [0010](0010-the-console-is-a-service.md) | The console is a service like any other: it ships its own logs and spans | accepted | 2026-11-17 | The collector never scraped the edge stack and nothing produced its spans, so the one component an operator stares at all day had no logs anywhere. The edge now emits OTLP itself: a zerolog→OTel JSON sink, an inbound span around the whole handler and a client span per outbound call, with the node passed in because a container's hostname is the container id |
| [0011](0011-the-platform-owns-the-log-schema.md) | The platform owns the log schema; app JSON stays in the body | accepted | 2026-11-18 | The collector keeps only platform-set attributes; a `null` field may never blind another service's logs; an app that wants structure ships OTLP (`skip_filelog`) |
| [0012](0012-failures-explain-and-back-off.md) | A failure names its cause, and an unchanged stack retries on a backoff | accepted | 2026-11-18 | Deploy errors name unhealthy services with task state and exit code, never the `depends_on` level or a transition word; retries reuse the revision and back off 2m→30m, cleared only by success or a new revision |

## Status values

- **proposed** — the problem and the options are written down; the code does not
  follow the decision yet.
- **accepted** — the code follows it. If only part has landed, say which part in
  the record (0001 names its layers and what was deliberately left alone).
- **superseded by NNNN** — kept, not deleted. The reasoning stays readable even
  after the decision changes.

## Writing one

- Number sequentially, `NNNN-kebab-title.md`. Never renumber, never reuse a number.
- Open with the status line and date: `Status: proposed · Date: YYYY-MM-DD`.
- **Context** states the problem with evidence — command output, file paths,
  measured counts — not a preference. **Decision** states the rule the code must
  follow. **Consequences** includes what gets worse; an ADR with no costs is
  advertising.
- Cite code by path relative to `pmcluster/` (`internal/manifest/compose_writer.go`),
  and prefer naming a function or symbol over a line range — line numbers drift
  and nothing checks them.
- Leave a **handoff checklist** when the decision is proposed and the work is not
  yours to finish; the checklist is what makes the next session cheap.

Link and file-reference integrity in this directory is checked by
`internal/docscheck` (`TestLocalMarkdownLinksResolve`,
`TestNoReferencesToMissingFiles`). Commands and flags named in an ADR are
deliberately *not* checked, so a proposal may name a command it intends to add.
