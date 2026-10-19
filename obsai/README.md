# ObsAI evidence slice

This directory contains the first runnable slice of Nextrum's in-cluster
observability companion. It has no model client, GitHub access, credentials,
or mutating endpoint yet. It fetches a bounded read-only log query from
Parseable, redacts common secrets and identifiers, aggregates normalized
templates, and serves the result as JSON and TSV.

The API is deliberately explicit about limits and unknown data:

- `GET /api/health` is local and never contacts Parseable.
- `GET /api/brief?window=2h` clamps the window to the configured maximum.
- Parseable queries are single-statement `SELECT`s with a hard row ceiling.
- A response includes `truncated`, `truncation_reasons`, `unknown`, and
  `unknown_fields`; it never presents a capped result as complete evidence.
- Missing service, severity, body, or timestamp fields are retained as
  `<unknown>` and reported instead of being silently discarded.

Run it locally without dependencies:

```sh
cd obsai
python -m unittest discover -s tests -v
python -m obsai
curl http://127.0.0.1:8080/api/health
curl http://127.0.0.1:8080/api/brief?window=2h
```

The local server binds to `127.0.0.1` by default. The container image sets its
bind address to `0.0.0.0`; in a deployment, put the API behind the pmcluster
Traefik authentication middleware and keep it on the private network. The
default Parseable URL is `http://observability_parseable:8000`, matching
the monitoring network name used by pmcluster. Set
`OBSAI_PARSEABLE_URL`, `OBSAI_PARSEABLE_STREAM`, and optionally
`OBSAI_PARSEABLE_AUTH_HEADER` for a deployed environment. No environment
variable is required for the health endpoint or for tests.

The `Dockerfile` builds a non-root image and exposes port 8080. `/state` is
reserved for the persistent template and analysis state that will be added in a
later slice. The current process does not write there.

The next bounded work is persistence and a pmcluster manifest. AI calls,
automatic triage, MCP, code checkout, and notifications remain separate phases
until the evidence contract is exercised against live data.
