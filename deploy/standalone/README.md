# Standalone monitoring deployment

This is an additive pmcluster monitoring mode for Docker/Compose hosts. It does
not initialize Swarm, reconcile apps, repair volumes, or expose restart/exec.
The existing Go console runs alongside a read-only observer, an OTel collector,
and a local Parseable backend. It is not the full Swarm management installation.

Use Crestline as the build/test host (`build-on-crestline.sh`), then distribute
the release binaries over SSH. Build static Linux amd64 binaries as `pmcluster` and `edge` beside the Dockerfile.
Copy this directory to `~/nextrum-monitoring` (mode0700), run `python3 init-env.py`
once, `docker build -t nextrum-pmcluster:standalone .`, and `docker compose up -d`.
Credentials are generated in `.env` mode0600, preserved across updates, never
committed. `PARSEABLE_HOSTNAME` is generated from the stable host name and is
used as the backend container hostname so Parseable manifest paths survive a
recreate. For an existing deployment, set it to the original hostname in the
first day manifest before adopting this Compose file; do not use a new
container ID. Collector offsets, backend history, daemon identity/accounting persist
in named volumes. Images use explicit release tags; rollback retains volumes.

Never run `. ./.env` or otherwise shell-source this file: `PARSEABLE_AUTH`
contains a space and the file contains credentials. Let Compose parse the env
file or use a parser that keeps values in memory and never prints them.

Only `127.0.0.1:18042` is published. Reach the console through an SSH tunnel:
`ssh -N -L 18042:127.0.0.1:18042 user@host`, then open
`http://localhost:18042/web/observability`. Loopback/SSH is the access gate;
console login is disabled for this private deployment. Do not expose this port
publicly or proxy it without adding authentication. All local processes on the
server can reach it. AI is disabled and no provider credential is provisioned.

`check-health.py` is the regular read-only status check. Run it from this
directory with `python3 check-health.py`; it discovers the Compose container
addresses, verifies the daemon and host inventory, reads the existing one-day
retention policy, checks fresh numeric host/container metrics, and loads every
observability tab. On Deshli it detects the host-run authenticated systemd
console and logs in through its live process environment without printing the
credentials. Use `--skip-mutation-check` only when the safe negative HTTP 405
assertion is unsuitable for a local policy. After a backend recovery, an
operator can additionally require persisted rows older than ten minutes with
`python3 check-health.py --history-minutes 15 --history-stream metrics
--history-stream logs`; this is opt-in so a newly installed host does not fail
its regular check before either stream has that history.

`verify.py` remains the one-time acceptance check. It runs authenticated inventory and negative authorization/mutation
checks, configures one-day retention for all three streams, and asserts live
container/host metrics and a synthetic stdout marker in Parseable and native UI.
For that check start a temporary Alpine container with Compose project label
`nextrum-check`, service label `probe`, emitting `NEXTRUM_LIVE_STDOUT_PROBE` after
collector discovery. Remove the probe after checking. The script never prints
application log bodies or credentials. The console renders node and container charts, logs (with live tail) and traces. Desired replicas, backups, certificate
management and Swarm node leadership are unknown/unavailable in this mode.

Install `python3 install-disk-guard.py`; every five minutes the user cron job
checks available space on the deployment filesystem. Below10GiB it pauses ONLY
the new collector. Inspect storage and resume manually with
`docker compose start collector`. Retention limits history age, not disk bytes;
this guard is a periodic safeguard, not a hard filesystem quota. CPU/memory and
Docker log rotation are bounded in Compose. Existing apps and any pre-existing
monitoring stack are unchanged. JSON-file logs at /var/lib/docker are required;
app-specific metrics/traces need SDK instrumentation directed to the new
collector and are not auto-instrumented by this deployment.

Rollback: `python3 install-disk-guard.py --remove`, then `docker compose down`
from this directory. Do not use `down -v`: volumes contain telemetry and durable
identity/accounting. This removes only the `nextrum-monitoring` project.

Current live tunnel ports: Crestline18142, Deshli18242, Damarkets18342.
Perhost .env CONSOLE_ORIGIN matches its current localhost tunnel URL. If the
local forwarded port changes, update CONSOLE_ORIGIN and recreate only edge
with docker compose up -d edge. Keep this gate consistent before provider setup.
