# Secrets & Configs (DB-backed)

pmcluster keeps secrets and configs in its SQLite store
(`~/.pmcluster/data.db`) — the DB is the source of truth — and mirrors them
into Docker Swarm as **content-addressed** objects. Secrets are
**AES-256-GCM encrypted** (key: `~/.pmcluster/.encryption_key`); configs are
plain text with a full **version history**. Both are managed from the CLI,
the REST API, and the operator console, and are consumable from any DSL
manifest through `env` references.

For the DSL-side rules (which keys accept references, strict YAML), see
[`dsl.md`](dsl.md). For how platform configs are rendered and snapshotted,
see the [whitepaper](whitepaper.md) and
[`control-loop-design.md`](control-loop-design.md).

## The four `env` reference kinds

Any whole-value `env` entry can reference the stores. Two families, each with a
value form and a location form — **singular is a value, `_path` is a location**:

| Reference | Resolves to | Use when |
|---|---|---|
| `secret_path(<name>)` | the **mount path** `/run/secrets/<name>` | the app reads a mounted file (requires `secrets: [<name>]`) |
| `secret(<name>)` | the secret's **value**, decrypted | the consumer only accepts the value via env and cannot read a file |
| `config_path(<name>)` | the **path** the config is mounted at, `<config_mount_root>/<name>.yaml` | the app reads a mounted config file (requires `configs: [<name>]`) |
| `config(<name>)` | the stored config's **content**, inline | a single-line value the app wants in the environment |

```yaml
services:
  web:
    secrets:
      - app_secret                      # the mount — without it the path points at nothing
    configs:
      - config_path(app_config)
    env:
      APP_SECRET_FILE: secret_path(app_secret)   # /run/secrets/app_secret
      APP_CONFIG_FILE: config_path(app_config)   # /etc/config/app_config.yaml
      APP_CONFIG_JSON: config(app_config)        # content inline (single line only)
      DB_URL: secret(db_url)                     # value, printed as-is
```

`settings(<name>)` and the older plural `secrets(<name>)` **no longer exist**:
`settings()` was removed in v0.2.224 and `secrets()` in v0.2.225. A manifest that
still carries either fails validation as a malformed reference — deliberately,
because an unknown `x(y)` would otherwise be written into the container
environment as a literal string and "work" until someone wondered why the value
was wrong.

Cluster settings (`domain`, `reconcile_interval`, `storage_nodes`, `backup_*`,
`config_mount_root`) are the platform's own knobs and are not addressable from a
manifest.

## Secrets

- Stored **AES-256-GCM encrypted** in `data.db`. The plaintext is shown
  **once** at creation; afterwards only the **sha256 hash** is displayed
  (`pmcluster secret list/show/verify`), and the UI lists every secret with
  its hash — never its value.
- An authenticated operator can still **decrypt a value on demand**
  (`GET /api/secrets/{name}/value`, or the *Reveal* button in the console,
  which asks for confirmation first) when they need to rotate a value
  elsewhere. Values can be **edited** in place (`PUT /api/secrets/{name}`)
  without recreating the secret.
- `pmcluster secret heal` verifies every secret decrypts with the current
  key, repairs stale hashes, and re-materializes missing swarm mirrors.
  `cluster update` runs the same rebuild-on-missing pass over every row, so a
  swarm object deleted by hand comes back on the next update — and a row that
  genuinely cannot be decrypted (sealed by an older `.encryption_key`) is
  reported there with the remedy rather than silently skipped. `secret heal
  --dry-run` is the report to run before believing the warning: it lists each
  row's status (ok / repaired / degraded / unrecoverable) without changing
  anything.

## Configs

- Stored in plain text with a full **version history**
  (`pmcluster config history` / `rollback`). Editing a config records the
  previous value; rolling back restores it. The console shows content,
  history, and a rollback button.
- **A promoted leader carries the newest 10 versions per config.** The
  survivor kit's `data.db` is windowed (see `internal/controlplane/kitdb.go`),
  so after a failover `config history` shows the last ten edits of each config,
  not its whole archive; the leader that recorded an older version keeps it, so
  a rollback further back than the window is a leader-side operation. The
  current config CONTENT always crosses, at every scope.

## Scopes: `cluster` vs `service`

- **`cluster`** — the platform's own templates and secrets. These are listed
  on the **Inventory** page's **Platform** tab in the console, which is the
  only page that shows both scopes at once. Settings keeps the create/edit
  forms and the **Apply to swarm** button (`POST /api/update`) that runs
  `cluster update` immediately so a change reaches the swarm side. Editing a
  cluster config re-stamps it with the current binary version, so the platform
  stacks render from it on the next update.
- Rendered platform configs (the post-substitution YAML actually sent to the
  Swarm) are snapshotted into the database by every cluster update and opened
  read-only from the Inventory **Platform** tab for a rendered config
  (`GET /api/cluster/rendered` — **operator/admin role only**: the rendered
  YAML embeds the observability backend's admin credentials).
- **`service`** — values belonging to one stack. The **Stacks** tab of the
  Inventory page lists them all with the owning stack, and a stack's own
  **Config** button (page `/stacks/<name>/config`) stays the focused view for
  that stack: it shows only that stack's service-scope rows (plus the shared,
  unattached ones) and never a cluster-scope row. Create forms prefill the
  `<stack>_` name prefix and record the owning stack.

## Content-addressed swarm mirrors

Every DB config/secret row is mirrored into the swarm as a content-addressed
object: `<name>_<sha256-first-8-hex>`.

- The same value always maps to the **same object**; a rotation mints a **new**
  object and the old, in-use secret stays mounted (Docker secrets are
  immutable — this is what makes rotation atomic).
- The DB is the index **and** holds the value: translate reads values from
  the swarm (swarm-first) and **rebuilds a missing object from the DB**; every
  `cluster update` runs a rebuild-on-missing repair pass.

## CLI cheat-sheet

Both `create` commands accept `--stack <name>` for service-scope values:

```bash
pmcluster secret create <name> [value] [--scope cluster|service] [--stack <name>]
pmcluster secret list / show <name> / verify <name> <value> / edit <name> / delete <name> / heal
pmcluster config create|list|get|edit|history|rollback|delete <name> [--scope ...] [--stack <name>]
pmcluster usage        # usage graph: which stacks reference each secret/config
```

## REST API

- `GET/POST /api/secrets` (`?scope=` / `?stack=` filters),
  `PUT/DELETE /api/secrets/{name}`, `GET /api/secrets/{name}/value`
- `GET/POST /api/configs` (`?scope=` / `?stack=` filters),
  `GET/PUT/DELETE /api/configs/{name}`,
  `GET /api/configs/{name}/versions`, `POST /api/configs/{name}/rollback`
- `POST /api/update` — apply platform config to the swarm

Full spec: [`pmcluster/docs/openapi.yaml`](../pmcluster/docs/openapi.yaml).
