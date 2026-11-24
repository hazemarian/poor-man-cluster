# ADR 0013 — Identity comes from the proxy, authorization from a table

**Status:** accepted (v0.2.396), closes C1 of the 2026-11-21 audit

## The problem

The console had two auth modes, and both were wrong in the same way: they decided
*access* at the wrong layer.

- With `EDGE_LOGIN_DISABLED` set, the session middleware attached a synthetic
  admin to **every** request. The reasoning was that Traefik's admin-auth gate
  stands in front of `/web/*`. That reasoning holds for the internet and fails
  everywhere else: the console listens on `traefik-net` and `monitoring-net`, and
  every app service with `expose.host` joins those networks. From inside one
  compromised app, `http://edge_pmcluster-edge:8042/web/...` was an unauthenticated
  admin panel — mint keys, reveal secrets, open a shell.
- With SSO, oauth2-proxy authenticated the person and then the console trusted it
  completely: any identity the proxy would pass (anyone in the org) was admin.

In both cases the console had no idea who was calling. The C3/H1/H4 fixes closed
the specific doors (labelled shell targets, host-gated bridge, POST-only shells);
this is the room behind them.

## The decision

**Two layers, each answering one question, and the answer to "what may you do" is
a row in the console's own database.**

1. *Who are you?* — SSO mode takes identity from the proxy's
   `X-Auth-Request-Email` / `-User` headers. Non-SSO mode takes it from the signed
   session cookie, as before. `EDGE_LOGIN_DISABLED` no longer means "no login"; it
   means "this console signs in through SSO, so do not offer the password form".
   It cannot make the console anonymous, and with SSO off the form stays — hiding
   the only way in is how a console locks itself out.
2. *What may you do?* — the `users` table: `role` (admin > operator > viewer) and
   `status` (`active` / `pending` / `disabled`). Every request re-reads the row, so
   a demotion or a removal takes effect on the next page load.

**First contact does not grant anything.** An SSO identity with no row gets one,
created `pending` with the `viewer` role, and every route redirects it to
`/web/pending` — a page that states the account is waiting and shows the email an
admin needs. No fragments, no counts, no stack names: a pending user has no role,
so it has nothing to render.

**Approval is an admin decision, in two places.** The console's Users page lists
pending identities with a role picker; `pmcluster ui approve <email> --role <role>`
does the same thing from the node for the case that matters — a fresh SSO console
where every identity is pending, so nobody can approve anybody from inside.

**The first admin is the password account, and it never auto-promotes.** GitHub
says who may reach the console at all (oauth2-proxy still restricts to the
configured org and repos — that gate is unchanged). It does not say who may run
the cluster.

## Consequences

- The console's SQLite is now **state, not cache**: it holds pending and active
  identities, so `DATA_DIR` is `/data` on the `pmui-data` volume in *both* modes.
  An in-memory store would forget every pending request across a restart, which is
  exactly when an operator is trying to find out who asked for access.
- A rejected identity is deleted, not marked: their next login starts a new
  request rather than reviving a decision somebody already made.
- Authorization now costs one SQLite read per request. That is the same cost the
  session path already paid, and it is why revocation is immediate.
- `pmcluster ui` is classified `scopeNode`: it reads and writes this machine's
  console volume, so gating it on leadership would fail exactly when it is needed.
- Losing the last active admin is still possible; it is now a documented recovery
  (approve from the node) rather than an open door.
