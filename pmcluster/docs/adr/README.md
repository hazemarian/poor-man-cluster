# Decision records

One file per decision that constrains the code, written when the decision is made
rather than reconstructed later. An ADR is not a design doc: it records the choice,
what was rejected and why, and what the choice costs — the design docs describe
what the system is.

| # | Decision | Status | Date | Settles |
|---|---|---|---|---|
| [0001](0001-engine-seam.md) | Keep Swarm behind a translation layer | accepted (layers 1–2 landed) | 2026-10-27 | Placement is expressed in platform terms in `internal/engine`; engine syntax lives only in `internal/engine/<engine>` |
| [0002](0002-cli-must-be-api-first.md) | The CLI is an API client with one local fallback | **proposed — not implemented** | 2026-10-29 | Every non-bootstrap command answers from the daemon API, so the answer does not depend on which node you are on |

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
  yours to finish. 0002 exists because a session ended mid-investigation; the
  checklist is what makes the next session cheap.

Link and file-reference integrity in this directory is checked by
`internal/docscheck` (`TestLocalMarkdownLinksResolve`,
`TestNoReferencesToMissingFiles`). Commands and flags named in an ADR are
deliberately *not* checked, so a proposal may name a command it intends to add.
