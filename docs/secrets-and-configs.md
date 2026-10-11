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

Any whole-value `env` entry can reference the stores:

```yaml
env:
  ADMIN_PASS: secrets(app_secret)     # stored secret → mount path /run/secrets/app_secret
                                      # (must also be listed in the service secrets: array)
  ADMIN_ENABLED: config(app_config)   # stored config content
  STORAGE_ROOT: settings(volume_root) # a live cluster setting's value
  DB_URL: secret(db_url)              # a stored secret's VALUE, printed as-is
```

| Reference | Resolves to | Use when |
|---|---|---|
| `secrets(<name>)` | the **mount path** `/run/secrets/<name>` | the app reads a mounted secret file (the secret must also be in the `secrets:` array) |
| `config(<name>)` | the stored config's **content** | embedding a config value in env |
| `settings(<name>)` | a live **cluster setting**'s value | e.g. `volume_root` |
| `secret(<name>)` | the secret's **value itself** | consumers that cannot read a mounted file (e.g. OpenObserve's root password env) |

Services can also mount configs as **files**:
`configs: [- config_path(<name>)]` (default path `/etc/<name>`).

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

## Configs

- Stored in plain text with a full **version history**
  (`pmcluster config history` / `rollback`). Editing a config records the
  previous value; rolling back restores it. The console shows content,
  history, and a rollback button.

## Scopes: `cluster` vs `service`

- **`cluster`** — the platform's own templates and secrets. These live on
  the **Settings** page in the console. Editing a cluster config re-stamps it
  with the current binary version, so the platform stacks render from it on
  the next update; the **Apply to swarm** button (`POST /api/update`) runs
  `cluster update` immediately so the change reaches the swarm side.
- Rendered platform configs (the post-substitution YAML actually sent to the
  Swarm) are snapshotted into the database by every cluster update and shown
  read-only on the **Settings** page under "Rendered cluster configs"
  (`GET /api/cluster/rendered` — **operator/admin role only**: the rendered
  YAML embeds the OpenObserve root credentials).
- **`service`** — values belonging to one stack, reached from the stacks list
  via the **Config** button (page `/stacks/<name>/config`). Create forms
  prefill the `<stack>_` name prefix and record the owning stack.

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
