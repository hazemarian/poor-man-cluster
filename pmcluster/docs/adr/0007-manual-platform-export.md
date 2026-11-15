# ADR 0007 — the control plane's restore point is a manual export that carries the key

**Status:** accepted (2026-11-15) · amends ADR 0005's consequence list · works against ADR 0003's split, on purpose

## Context

ADR 0005 removed the control-plane backup *service* along with the data kit. That was right for
the mechanism and wrong as a statement about durability: with the service gone, nothing
archived the control plane, and `backup_offsite` skips the old `pmcluster-ctlplane-*.tar.gz`
objects as leftovers. The mTLS mirror gives every manager the state, so losing one manager —
or even the leader — costs nothing. It gives nothing back when:

- all three manager disks die (the mirror's source and its copies go together),
- an upgrade corrupts `data.db` on every manager before anyone notices,
- a cluster is torn down with `--purge` and the volumes go with it.

In each of those the user list, the settings, the stack definitions and every secret ciphertext
are gone, and no artifact exists to restore from.

The automated answer — a timer, or a service in the stack — is what ADR 0005 deleted, and for
good reasons: it is an autonomous object moving platform state around, its restore path has no
consumer since promotion stopped restoring from a kit, and a scheduled job that silently stops
working looks exactly like one that works.

## Decision

**One operator-triggered export, on the leader, written offsite.** A button in the console
(`POST /web/backups/platform`) and `pmcluster backup platform` drive the same code
(`backups.PlatformExporter`).

- **Leader only.** A follower's `data.db` is a mirror; exporting it produces a restore point
  whose provenance nobody can name. A non-leader answers 409 with that sentence.
- **`data.db` AND `.encryption_key`, in one archive.** This deliberately breaks ADR 0003's
  "key and ciphertext never share a blob" rule, and the reason is the artifact's whole purpose:
  a control-plane export without its key cannot decrypt one stored secret, so it would look
  like a backup and restore into a cluster nobody can read. The split existed to stop the
  *automatic, cluster-internal* channel from carrying both; a human-initiated archive written
  to an operator-owned bucket is a different thing, and it is the only way the artifact is
  self-sufficient.
- **`MANIFEST.json` inside**: kind, when, which host, which build (`version`/`commit`), and a
  sha256 + mode per member, hashed as the bytes are written so it describes what actually
  landed rather than what was on disk beforehand.
- **Refuses to write a partial archive.** No key, or no database, is an error and no file — a
  half export is worse than none, because it reads as complete.
- **Nothing reads it back automatically.** No schedule, no reconcile, no promotion path. It
  exists for a human restoring after a disaster.

## Consequences

- The platform regains a disaster-recovery restore point without regaining an autonomous
  component. If nobody presses the button, there is still no backup — that is the accepted
  cost of not running a service for it, and it is why the button lives on the backups page
  next to the volume trigger rather than in a settings sub-page.
- `doctor` does not check for a recent platform export, because there is no schedule to be
  late against. If this becomes a habit worth enforcing, the check is "age of the newest
  `pmcluster-platform-*` object", not "did the timer fire".
- Restoring is manual and documented: unpack `data.db` and `.encryption_key` into the data dir
  of a node that will lead, then `cluster up`/`update` as usual. The key travels with the
  archive, so no separate step can be forgotten.
- One more route on the daemon API and one more button in the console; both are wired through
  the same exporter, so the CLI and the page cannot drift.
