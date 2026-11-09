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
// secrets) existed only on that one disk. The operator's call: revisions do not
// need to cross the fleet — a promoted leader re-renders them from the repo —
// while auth, settings, credential ciphertexts and the stack LIST do.
//
// So the kit is built from a fresh database holding the schema (all migrations)
// and the rows of the tables below. VACUUM INTO would not do: it copies
// everything, which is the thing that does not fit.
//
// The full data.db still rides the tarball/offsite backup path, so disaster
// recovery keeps the revision history; only the Raft kit is slimmed.

// authoritativeTables are copied into the kit, in copy order (parents before
// children, because the export enforces foreign keys). Each one is here because
// a promoted leader cannot reconstruct it:
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
//   - webhook_sources — HMAC shared secrets (ciphertext), which CI needs to
//     keep authenticating against the new leader.
//   - registries — `docker login` credentials (ciphertext) for private images;
//     a promoted leader that cannot pull cannot deploy.
//   - cluster_settings — install-time truth: domain, TLS mode/paths, volume
//     root, backup store placement, OO admin email, last provisioned versions.
//   - configs — cluster- and service-scope config CONTENTS (templates and
//     rendered snapshots) and their rendered_hash; the platform re-renders from
//     these, so they are state, not history.
//   - config_versions — previous config contents. AMBIGUOUS, kept deliberately:
//     for service-scope configs an operator edited in the console this history
//     is not in any repo and cannot be rebuilt, and dropping it would take away
//     `config rollback` on the promoted leader. It is also the second-largest
//     table after revisions (258 KB live → 36 KB gzipped), so it was the one
//     worth weighing. Correctness beats bytes: 36 KB is not what breaks the cap.
//   - secrets — AES-GCM payloads plus sha256 for display; referenced by running
//     services, unreadable if their ciphertext is lost.
//   - site_certs — which Swarm secret (cert_vN/key_vN) is today's site cert,
//     its validity window and hashes. The PEM lives in Swarm; this metadata is
//     what makes the console warn before expiry.
//   - stack_failover — "this stack was relocated off a failed node and runs on
//     the last offsite backup". Small, and it is the only record of an unacked
//     degraded state; losing it would leave the new leader silently claiming
//     data is current.
var authoritativeTables = []string{
	"users",
	"managed_credentials",
	"stacks",
	"webhook_sources",
	"registries",
	"cluster_settings",
	"configs",
	"config_versions",
	"secrets",
	"site_certs",
	"stack_failover",
}

// excludedTables names every table that stays OUT of the kit and why. It is
// kept as data (not prose above) so a test can hold the schema to it: a future
// migration that adds a table without classifying it fails the build's tests
// instead of silently vanishing from the survivor kit.
//
// schema_version and sqlite_sequence are absent for structural reasons rather
// than policy, and the same test allows for them: the export creates its schema
// by running the migrations, so schema_version is written by that run, and
// sqlite_sequence is maintained by SQLite itself for AUTOINCREMENT tables.
var excludedTables = map[string]string{
	// The whole point of the change. Revisions are the deploy history: the
	// source DSL, the rendered compose, payload JSON, ~6 KB a piece and
	// hundreds of them. Re-render from the repo instead. A promoted leader
	// serves the stack LIST correctly and reports "no stored revision" for
	// these stacks until they are re-deployed (see stacks.Sync).
	"stack_revisions": "deploy history (source + rendered compose); re-rendered from the repo, 3.7 MB of the 5.2 MB live file",
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
	counts, err := store.ExportDB(k.DBPath(), dst, authoritativeTables)
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	k.Log.Debug().Str("component", "controlplane").Interface("rows", counts).
		Msg("control-plane kit database exported from the authoritative tables")
	return dst, cleanup, nil
}
