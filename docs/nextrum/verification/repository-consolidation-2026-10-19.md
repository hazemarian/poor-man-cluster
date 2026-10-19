# Local repository consolidation — 2026-10-19

Branch: `nextrum/three-node-observability`, local only at the user's request.
Latest fetched `origin/main`: `e91f3e4e69983a77767e9f05ff3a2c854a808a86`.
That revision is an ancestor of the branch; all ten existing Nextrum commits
through `f047df6` are preserved. No upstream merge or history rewrite was needed.

This consolidation adds the previously uncommitted application changes,
standalone/fleet deployment tools, documentation, static verification evidence,
UI preview images and parked ObsAI prototype to the application repository.
The parent workspace remains intact. Chat transcripts, agent/browser state,
credentials, runtime state, caches and compiled binaries are excluded.

Checks performed:

- All 23 application files match their pre-consolidation SHA-256 baseline.
- Relative Markdown link validation and staged `git diff --check` pass.
  Trailing whitespace in the captured regression HTML log was normalized;
  assertions and failure output are preserved.
- Executor reports Python AST parsing for 21 files, 16 passing ObsAI tests,
  and Compose config validation. No Go tests or builds were rerun locally.
- Source/config/document secret-pattern scans found no matching credentials.
  OCR text from all 11 preview images had no matching credential patterns.
  These are heuristic checks, not proof that arbitrary secrets cannot exist.
- Independent read-only review checked ancestry, code preservation, packaging,
  links, static logs and documentation claims; no actionable findings remained.
- Previously deployed Go race/vet and live browser/collector evidence is retained
  in the dated reports; consolidation does not redeploy any host.

The upstream remote is preserved. Nothing is pushed; no pull request, tag or
release is created by this consolidation.
