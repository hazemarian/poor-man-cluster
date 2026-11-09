package controlplane

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// The data.db the kit carries is an EXPORT of the control plane's authoritative
// tables, not the leader's database file.
//
// Why: Docker caps a config at 500 KiB and the survivor kit is published as a
// Docker config. The leader's data.db grew to 5.2 MB, 3.7 MB of it
// stack_revisions; gzipped the kit was 538 KB, so every publish was refused and
// nothing created since the last accepted snapshot (API keys, settings,
// secrets) existed only on that one disk. The operator's call: a promoted
// leader needs the authoritative rows plus a SHORT rollback window, not the
// archive — config history and deploy history grow one row per edit and per
// deploy, forever, while the kit has a hard 512,000-byte ceiling.
//
// So the kit is built from a fresh database holding the schema (all migrations)
// and the rows selected by kitTables. VACUUM INTO would not do: it copies
// everything, which is the thing that does not fit.
//
// The full data.db still rides the tarball/offsite backup path, so disaster
// recovery keeps the whole history; only the Raft kit is windowed.

// The kit's history retention caps — the operator's numbers, in ONE place so
// neither can drift on its own:
//
//   - the newest 5 revisions per stack (plus the current one — see below), so a
//     promoted leader can roll back a deploy and reconcile against the rendered
//     manifest without a re-deploy first;
//   - the newest 10 versions per config name, so `config rollback` still has
//     something to roll back to on the new leader.
//
// They are budgets, not preferences: at 18 stacks and 19 configs the capped
// history is a few hundred KB uncompressed and the packed kit stays around a
// quarter of the Docker ceiling. Raising either one is a size decision — the
// guard test in kitdb_test.go packs 500 revisions and 200 versions per group
// and fails if the kit stops fitting.
const (
	kitStackRevisionCap = 5
	kitConfigVersionCap = 10
)

// kitTables are the tables copied into the kit, in copy order (parents before
// children, because the export enforces foreign keys). Each one is here because
// a promoted leader cannot reconstruct it, and each carries a comment saying
// why — the reasons are the design, not decoration:
//
//   - users — API tokens (argon2id token_hash + token_id index, role, per-stack
//     scope, last_used_at). Without them every key minted since the last
//     snapshot 401s on the new leader; this is the live defect's sharpest edge.
//   - managed_credentials — AES-GCM password ciphertexts for bundled stacks
//     (Traefik dashboard, OpenObserve admin). Generated on this cluster, not
//     derivable from anything.
//   - stacks — the stack LIST the operator asked to keep: name, repo_url,
//     deploy source_file, current_revision pointer, last_error history. Losing
//     it would make 18 running stacks invisible to the console.
//   - stack_revisions — WINDOWED to the newest 5 per stack plus the row
//     current_revision points at. The pointer is what tells the new leader
//     which manifest is live, and the row behind it is the rendered compose
//     every reconcile drift check and `stack rollback` needs; without it a
//     promoted leader knows stack X is on revision 42 and has no row 42, so
//     both are dead until someone re-deploys. The current row is kept even
//     when it is older than the newest five (a rollback leaves newer rows in
//     history). The rest of the history stays behind: it is re-rendered from
//     the repo on the next deploy, and a whole-table copy is what dragged the
//     kit over the limit in the first place.
//   - webhook_sources — HMAC shared secrets (ciphertext), which CI needs to
//     keep authenticating against the new leader.
//   - registries — `docker login` credentials (ciphertext) for private images;
//     a promoted leader that cannot pull cannot deploy.
//   - cluster_settings — install-time truth: domain, TLS mode/paths, volume
//     root, backup store placement, OO admin email, last provisioned versions.
//   - configs — cluster- and service-scope config CONTENTS (templates and
//     rendered snapshots) and their rendered_hash; the platform re-renders from
//     these, so they are state, not history.
//   - config_versions — WINDOWED to the newest 10 per config name (row ids
//     preserved, because a rollback names the row by id). The table is the one
//     unbounded console-edit log, ~36 KB gzipped live; the whole table would
//     grow forever inside a fixed ceiling. Ten versions is the operator's
//     rollback window on a promoted leader; older versions remain only on the
//     leader that recorded them, so a rollback further back is a leader-side
//     operation.
//   - secrets — AES-GCM payloads plus sha256 for display; referenced by running
//     services, unreadable if their ciphertext is lost.
//   - site_certs — which Swarm secret (cert_vN/key_vN) is today's site cert,
//     its validity window and hashes. The PEM lives in Swarm; this metadata is
//     what makes the console warn before expiry.
//   - stack_failover — "this stack was relocated off a failed node and runs on
//     the last offsite backup". Small, and it is the only record of an unacked
//     degraded state; losing it would leave the new leader silently claiming
//     data is current.
//   - github_app — the App id, installation id and AES-GCM private key the
//     cluster mints installation tokens with. A promoted leader that lost it
//     could not log in to GHCR (private pulls stop) and could not serve the
//     Actions token broker, while every workflow that deleted its PAT now has
//     no other credential.
//   - github_broker_secrets — the per-repo HMAC secrets those workflows hold.
//     Same argument: they are generated here and cannot be reconstructed, and
//     a promoted leader that forgot them would 401 every CI job at once.
//
// The zero Selector is CopyAll: a table is copied verbatim unless it says
// otherwise, so adding an authoritative table is one line here.
var kitTables = []store.CopyPlan{
	{Table: "users"},
	{Table: "managed_credentials"},
	{Table: "stacks"},
	// After stacks: stack_revisions.stack_name is a foreign key into it.
	{Table: "stack_revisions", Selector: store.CopyNewestStackRevisions, Keep: kitStackRevisionCap},
	{Table: "webhook_sources"},
	{Table: "registries"},
	{Table: "cluster_settings"},
	{Table: "configs"},
	// After configs: config_versions.config_id is a foreign key into it.
	{Table: "config_versions", Selector: store.CopyNewestConfigVersions, Keep: kitConfigVersionCap},
	{Table: "secrets"},
	{Table: "site_certs"},
	{Table: "stack_failover"},
	{Table: "github_app"},
	{Table: "github_broker_secrets"},
}

// excludedTables names every table that stays OUT of the kit entirely, and why.
// It is kept as data (not prose above) so a test can hold the schema to it: a
// future migration that adds a table without classifying it fails the build's
// tests instead of silently vanishing from the survivor kit.
//
// schema_version and sqlite_sequence are absent for structural reasons rather
// than policy, and the same test allows for them: the export creates its schema
// by running the migrations, so schema_version is written by that run, and
// sqlite_sequence is maintained by SQLite itself for AUTOINCREMENT tables.
var excludedTables = map[string]string{
	// The audit log of backup runs, one row per archive on disk (2775 rows,
	// 545 KB with its filename index). The backup path re-discovers archives
	// from the filesystem, and offen/offsite own the real record.
	"backups": "on-demand/scheduled backup audit log; archives are re-discovered from disk",
	// Per-delivery webhook history for the console. Instrumentation.
	"webhook_deliveries": "webhook delivery history; the console's log view, not control state",
	// The reconcile loop's own output, rewritten every pass from live Swarm.
	"stack_status": "reconcile bookkeeping (status badge cache); rebuilt on the first pass after promotion",
	// Volatile per-node sampling from the collector.
	"node_stats": "per-node resource samples; volatile local telemetry, repopulated by the collector",
}

// exportedDBPath writes the authoritative subset of this node's data.db into a
// fresh file in a scratch directory and returns its path plus a cleanup func.
// The file is built outside DataDir on purpose: a temporary file inside the
// data dir would race the kit walk (which packs everything it finds) and would
// make the kit look dirty on every tick.
func (k *Kit) exportedDBPath() (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "pmcluster-kit-db-")
	if err != nil {
		return "", func() {}, fmt.Errorf("create kit db dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	dst := filepath.Join(dir, "data.db")
	counts, err := store.Export(k.DBPath(), dst, kitTables)
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	k.Log.Debug().Str("component", "controlplane").Interface("rows", counts).
		Msg("control-plane kit database exported from the authoritative tables")
	return dst, cleanup, nil
}
