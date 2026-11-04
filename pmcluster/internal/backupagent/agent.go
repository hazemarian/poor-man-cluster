// Package backupagent is the node-local half of the backup pipeline.
//
// It runs as a privileged one-shot (systemd timer → `pmcluster backup-agent
// run`) rather than as a Swarm service, because the only way to take a
// consistent image of a live volume without stopping anything is a
// freeze-and-snapshot on the host: fsfreeze + LVM thin snapshot. A Swarm task
// cannot do that — it is rescheduled, it cannot hold a lock across the run,
// and it has no reliable ordering with the agent's own lifecycle.
//
// The agent is deliberately engine-agnostic: it never reads a database. It
// captures whatever files the stack owns, one artifact per stack, and records
// how the image was obtained.
//
// # Deliberately not implemented: Postgres PITR
//
// The snapshot restores to the instant it was taken, so RPO is the hourly
// interval and the finest undo is an hour. Point-in-time recovery would make
// both arbitrary — a base image plus a continuous WAL stream replayed to a
// timestamp. It is a FUTURE PLAN, not a gap somebody forgot:
//
//   - base: the snapshot taken here is already a valid base (crash-consistent,
//     and Postgres replays its own tail on start);
//   - stream: pg_receivewal inside the stack's Postgres container, writing into
//     <stack>/_wal over the container's local trust socket with its own
//     POSTGRES_USER — no password, no new role, no pg_hba change, which is the
//     only reason it is viable on a platform that does not hold database
//     credentials;
//   - prerequisite, not follow-up: max_slot_wal_keep_size is -1 (unlimited) on
//     our instances today. A receiver that dies while a slot exists makes
//     Postgres retain WAL forever and fill the disk, so the cap and the
//     pg_replication_slots.wal_status alert must land BEFORE the slot exists;
//   - restore: extract the base, point restore_command at the shipped WAL, write
//     recovery.signal, set recovery_target_time, start once.
//
// Limits to state before anyone builds it: WAL replays only into the same major
// version, PITR rewinds a whole instance rather than a table, and MySQL/MariaDB
// need their own binlog equivalent. See docs/storage-and-databases.md,
// "Future plan: Postgres point-in-time recovery".
package backupagent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
)

// Prefix is the member prefix inside every artifact.
//
// It is NOT cosmetic: the archives the offen agents produced carried
// "/backup/data/<stack>/..." members, and internal/backups/browse.go strips
// exactly that prefix on browse and restore. Writing the same prefix keeps
// browse, restore, prune-to-snapshot, the console and the CLI working with no
// changes at all — the artifact is narrower (one stack), the wire format is
// identical.
const Prefix = "backup/data"

// PoolAbortPercent is the thin-pool data percentage at which a run aborts.
// A thin snapshot that fills its pool is invalidated by the kernel and cannot
// be read back, so stopping early with a loud failure beats shipping an image
// that silently went away mid-copy.
const PoolAbortPercent = 80.0

// StackPlan is one stack the node is responsible for.
type StackPlan struct {
	// Name is the stack (and its directory under the volume root).
	Name string `json:"name"`
	// Volumes narrows what is captured, as paths relative to the stack
	// directory. Empty means the whole stack directory.
	Volumes []string `json:"volumes,omitempty"`
}

// Plan is the leader's instruction set for one node. It is cached on the node
// so a run never depends on the leader being reachable: the agent prefers the
// cached copy, asks the leader for a refresh, and proceeds either way.
type Plan struct {
	Node        string      `json:"node"`
	Stacks      []StackPlan `json:"stacks"`
	GeneratedAt int64       `json:"generated_at,omitempty"`
	// VolumeRoot is the host directory the plan's stacks live under.
	VolumeRoot string `json:"volume_root,omitempty"`
}

// StackNames returns the planned stack names, for diffing against disk.
func (p Plan) StackNames() []string {
	out := make([]string, 0, len(p.Stacks))
	for _, s := range p.Stacks {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

// Stack is one stack's outcome.
type Stack struct {
	Stack  string `json:"stack"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	// Archive is the filename (leaf) written into the archive dir.
	Archive string `json:"archive,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
	Files   int    `json:"files,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	// Verified is true only when the archive was reopened and its databases
	// answered an integrity question.
	Verified bool   `json:"verified"`
	Engine   string `json:"engine,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Result is what the run reports back. It is written to disk first and posted
// to the leader best-effort: the artifact is the record, the report is
// enrichment.
type Result struct {
	// Pruned is what retention removed after the run.
	Pruned  backups.PruneResult `json:"pruned,omitempty"`
	Node    string              `json:"node"`
	Started int64               `json:"started_at"`
	Ended   int64               `json:"ended_at"`
	Stacks  []Stack             `json:"stacks"`
	// Error is set when the run as a whole failed (no snapshot, pool abort).
	Error string `json:"error,omitempty"`
	// Snapshot records how consistency was obtained for this run.
	Snapshot backups.SnapshotFacts `json:"snapshot"`
}

// Succeeded reports whether every planned stack produced a verified artifact.
func (r Result) Succeeded() bool {
	if r.Error != "" {
		return false
	}
	for _, s := range r.Stacks {
		if !s.OK {
			return false
		}
	}
	return true
}

// PlanFromDisk derives a plan from the volume root itself: one stack per
// directory, which is exactly the layout the platform's bind mounts create
// (/var/stack/data/<stack>/<volume>/…).
//
// It exists so an hourly run does not depend on the control plane publishing a
// plan. The leader's plan stays authoritative when it is available — it carries
// attribution the filesystem cannot know, such as which volumes belong to a
// stack that was moved — but a backup that stops because a leader is down is not
// a backup. lost+found is skipped: it belongs to no stack.
func PlanFromDisk(root string) (Plan, error) {
	plan := Plan{VolumeRoot: root}
	entries, err := os.ReadDir(root)
	if err != nil {
		return plan, fmt.Errorf("read %s: %w", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "lost+found" {
			continue
		}
		plan.Stacks = append(plan.Stacks, StackPlan{Name: e.Name()})
	}
	return plan, nil
}

// Verifier is the optional pre-flight a snapshotter may implement. Run calls it
// before freezing anything: a node that cannot prove its volume root is the
// volume it is configured to snapshot must fail loudly, not archive the wrong
// tree.
type Verifier interface {
	Verify(ctx context.Context, root string) error
}

// Snapshotter turns the live volume root into a tree nothing writes to.
// Production is LVMSnapshotter; NoSnapshot exists for tests and --dry-run.
type Snapshotter interface {
	// Freeze blocks writes on the filesystem holding root. The window must be
	// milliseconds: everything on the node stalls inside it.
	Freeze(ctx context.Context, root string) error
	// Thaw releases the freeze. It must run even when Freeze failed half-way.
	Thaw(ctx context.Context, root string) error
	// Snapshot captures root and returns a readable directory plus a token
	// Drop later accepts.
	Snapshot(ctx context.Context, root string, at time.Time) (dir, token string, err error)
	// Usage reports thin-pool data percentage, or -1 when not applicable.
	Usage(ctx context.Context) (float64, error)
	// Drop releases the snapshot.
	Drop(ctx context.Context, token string) error
	// Kind labels the mechanism in the manifest ("lvm", "none").
	Kind() string
}

// Agent performs one run.
type Agent struct {
	// Node is the swarm node id. It must be the id, not the hostname: the
	// legacy archiveNodeID parser in internal/stacks splits the name on the
	// first '-' and swarm ids contain none.
	Node       string
	VolumeRoot string
	// ArchiveDir is where artifacts and their sidecars land (the host's
	// /var/stack/backup today, so discovery needs no change).
	ArchiveDir string
	Snapshot   Snapshotter
	// Now is overridable in tests.
	Now func() time.Time
	// Logf receives progress lines.
	Logf func(format string, args ...any)
	// Ship, when set, is called per artifact after it is written and
	// verified: the node uploads its own archives.
	Ship func(ctx context.Context, archivePath string) error
	// Verify inspects one artifact. Nil means VerifyArchive with the default
	// rules.
	Verify func(archivePath, stack string) (engine, detail string, err error)
	// Retention, when set, prunes the archive directory after a run. Zero value
	// means "leave the archives alone" — a test or a one-off run must not delete
	// history just because it happened to execute.
	Retention *backups.Retention
	// DryRun walks the plan and reports what it would do without freezing,
	// snapshotting or writing anything.
	DryRun bool
	// snapLV is the snapshot token for the run in progress, recorded in each
	// artifact's manifest.
	snapLV string
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

func (a *Agent) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}

// UnplannedDirs lists directories under the volume root that the plan does not
// mention. They are never archived implicitly — a decommissioned stack's
// leftover directory must not quietly enter the backup set — but they are
// reported so an operator can delete them or add the stack back.
func UnplannedDirs(volumeRoot string, plan Plan) []string {
	entries, err := os.ReadDir(volumeRoot)
	if err != nil {
		return nil
	}
	planned := make(map[string]bool, len(plan.Stacks))
	for _, s := range plan.Stacks {
		planned[s.Name] = true
	}
	var out []string
	for _, e := range entries {
		// lost+found exists on every freshly mkfs'd volume and belongs to no
		// stack: warning about it on a brand-new node would train the operator
		// to ignore the warning that matters.
		if !e.IsDir() || planned[e.Name()] || e.Name() == "lost+found" {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// Run executes the plan: one freeze, one snapshot, one artifact per stack.
func (a *Agent) Run(ctx context.Context, plan Plan) (Result, error) {
	res := Result{Node: a.Node, Started: a.now().Unix()}
	if a.Node == "" {
		return res, errors.New("backup-agent: node id is required")
	}
	if a.VolumeRoot == "" || a.ArchiveDir == "" {
		return res, errors.New("backup-agent: volume root and archive dir are required")
	}
	if a.Snapshot == nil {
		return res, errors.New("backup-agent: no snapshotter configured")
	}
	if len(plan.Stacks) == 0 {
		a.logf("backup-agent: plan holds no stacks for node %s — nothing to do", a.Node)
		return res, nil
	}
	res.Snapshot.Kind = a.Snapshot.Kind()
	if a.Snapshot.Kind() == "" {
		res.Snapshot.Kind = "unknown"
	}

	if a.DryRun {
		for _, s := range plan.Stacks {
			res.Stacks = append(res.Stacks, a.dryRunStack(s))
		}
		res.Ended = a.now().Unix()
		return res, nil
	}

	if v, ok := a.Snapshot.(Verifier); ok {
		if err := v.Verify(ctx, a.VolumeRoot); err != nil {
			res.Error = err.Error()
			res.Ended = a.now().Unix()
			return res, fmt.Errorf("backup-agent: %w", err)
		}
	}

	if err := os.MkdirAll(a.ArchiveDir, 0o755); err != nil {
		return res, fmt.Errorf("backup-agent: archive dir: %w", err)
	}

	// Drop snapshots a previous run could not remove. Deliberately before the
	// freeze: a stale snapshot holds pool blocks and its mount point would
	// collide with this run's.
	if cleaner, ok := a.Snapshot.(interface {
		RemoveStale(context.Context) ([]string, error)
	}); ok {
		if gone, serr := cleaner.RemoveStale(ctx); serr != nil {
			a.logf("backup-agent: stale snapshot cleanup: %v", serr)
		} else if len(gone) > 0 {
			a.logf("backup-agent: removed %d snapshot(s) left by an interrupted run: %s", len(gone), strings.Join(gone, ", "))
		}
	}

	// Freeze, snapshot, thaw. The thaw is deferred so a snapshot failure can
	// never leave the node's filesystem frozen — that would stall every stack
	// on the node, which is far worse than a missed backup.
	// Phase timings are logged separately because "wrote blocked for 17m" was
	// once reported with no way to tell WHICH step held the freeze: the fsfreeze
	// call itself (flush), or lvcreate. One production run took seventeen minutes
	// and the number alone could not say why.
	freezeStarted := a.now()
	if err := a.Snapshot.Freeze(ctx, a.VolumeRoot); err != nil {
		res.Error = fmt.Sprintf("freeze failed: %v", err)
		res.Ended = a.now().Unix()
		return res, fmt.Errorf("backup-agent: freeze %s: %w", a.VolumeRoot, err)
	}
	res.Snapshot.FrozenAt = a.now().UnixNano()
	freezeCost := a.now().Sub(freezeStarted)
	at := a.now()

	snapDir := ""
	token := ""
	snapErr := error(nil)
	func() {
		defer func() {
			if terr := a.Snapshot.Thaw(ctx, a.VolumeRoot); terr != nil {
				// Not fatal to the artifacts, but it must be visible.
				a.logf("backup-agent: THAW FAILED (%v) — the node may still be frozen", terr)
			}
			res.Snapshot.ThawedAt = a.now().UnixNano()
		}()
		snapDir, token, snapErr = a.Snapshot.Snapshot(ctx, a.VolumeRoot, at)
	}()
	snapCost := a.now().Sub(at)
	if freezeCost > 2*time.Second || snapCost > 2*time.Second {
		a.logf("backup-agent: SLOW WINDOW — fsfreeze %s, lvcreate+thaw %s", freezeCost.Round(time.Millisecond), snapCost.Round(time.Millisecond))
	}
	if snapErr != nil {
		// Deliberately no fallback to tarring the live tree: that is exactly
		// the bug this agent exists to remove.
		res.Error = fmt.Sprintf("snapshot failed: %v", snapErr)
		res.Ended = a.now().Unix()
		return res, fmt.Errorf("backup-agent: snapshot: %w", snapErr)
	}
	res.Snapshot.LV = token
	a.snapLV = token
	defer func() {
		if err := a.Snapshot.Drop(context.WithoutCancel(ctx), token); err != nil {
			a.logf("backup-agent: drop snapshot %s: %v", token, err)
		}
	}()

	for _, s := range plan.Stacks {
		st := a.archiveStack(ctx, snapDir, s, at)
		res.Stacks = append(res.Stacks, st)
		if st.Archive != "" {
			if a.Ship != nil {
				p := filepath.Join(a.ArchiveDir, st.Archive)
				if err := a.Ship(ctx, p); err != nil {
					st.OK = false
					st.Reason = fmt.Sprintf("ship: %v", err)
					res.Stacks[len(res.Stacks)-1] = st
				}
			}
		}
	}

	// Pool guard: a thin snapshot approaching its pool limit is invalidated by
	// the kernel, so the artifacts above cannot be trusted even though they
	// were written. Report the run as failed and let retention keep them; the
	// operator sees a failed run with a reason instead of a silent gap.
	if pct, err := a.Snapshot.Usage(ctx); err == nil && pct >= 0 {
		res.Snapshot.PoolPercent = pct
		if pct >= PoolAbortPercent {
			res.Error = fmt.Sprintf("thin pool at %.1f%% (limit %.0f%%) — snapshot may have been invalidated", pct, PoolAbortPercent)
			a.logf("backup-agent: %s", res.Error)
		}
	}
	res.Ended = a.now().Unix()

	// Retention runs on the way out, after the artifacts are written and after
	// the pool guard has judged the run: pruning first would delete a good
	// artifact to make room for a bad one.
	if a.Retention != nil {
		pruned, perr := backups.PruneArtifacts(a.ArchiveDir, *a.Retention, a.now(), false)
		res.Pruned = pruned
		if perr != nil {
			// Retention failing must not fail a run that produced good artifacts.
			a.logf("backup-agent: retention: %v", perr)
		} else if len(pruned.Deleted) > 0 {
			a.logf("backup-agent: retention removed %d artifact(s), freed %s", len(pruned.Deleted), humanBytes(pruned.FreedBytes))
		}
	}

	if res.Error != "" {
		return res, errors.New(res.Error)
	}
	return res, nil
}

func (a *Agent) dryRunStack(s StackPlan) Stack {
	dir := filepath.Join(a.VolumeRoot, s.Name)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return Stack{Stack: s.Name, OK: false, Reason: "not present on this node"}
	}
	return Stack{
		Stack:  s.Name,
		OK:     true,
		Detail: fmt.Sprintf("would archive %s → %s", dir, backups.ArtifactName(s.Name, a.Node, a.now())),
	}
}

// archiveStack writes one stack's artifact plus its sidecar manifest.
func (a *Agent) archiveStack(ctx context.Context, snapDir string, s StackPlan, at time.Time) Stack {
	out := Stack{Stack: s.Name}
	src := filepath.Join(snapDir, s.Name)
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		out.Reason = "not present in the snapshot (moved away or never deployed on this node)"
		return out
	}
	name := backups.ArtifactName(s.Name, a.Node, at)
	dst := filepath.Join(a.ArchiveDir, name)
	// member is the archive-relative base; addTree appends the path relative to
	// the stack's PARENT, so the stack name appears exactly once.
	files, bytes, sha, err := writeTarGz(ctx, src, Prefix, dst, s.Volumes)
	if err != nil {
		_ = os.Remove(dst)
		out.Reason = err.Error()
		return out
	}
	out.Archive, out.Files, out.Bytes, out.SHA256 = name, files, bytes, sha
	out.OK = true
	// SizeBytes describes the artifact on disk (compressed), while Bytes is
	// the payload that went in. Conflating them makes an integrity check on
	// the archive compare a tarball against a database.
	size := bytes
	if st, serr := os.Stat(dst); serr == nil {
		size = st.Size()
	}

	engine, detail, verr := "", "", error(nil)
	verify := a.Verify
	if verify == nil {
		verify = VerifyArchive
	}
	if verify != nil {
		engine, detail, verr = verify(dst, s.Name)
	}
	m := backups.Manifest{
		Stack:      s.Name,
		Node:       a.Node,
		CreatedAt:  at.Unix(),
		Archive:    name,
		SizeBytes:  size,
		SHA256:     sha,
		VolumeRoot: a.VolumeRoot,
		Prefix:     Prefix,
		Files:      files,
		Bytes:      bytes,
		Snapshot:   backups.SnapshotFacts{Kind: a.Snapshot.Kind(), LV: a.snapLV},
		Verify:     backups.VerifyFacts{Verified: verr == nil, Engine: engine, Detail: detail},
	}
	if verr != nil {
		// The artifact stays on disk — a failed verification is a reason to
		// investigate, not to delete the only copy — but the run is NOT green.
		// "Succeeded" is a claim about restorability, not about file size.
		m.Verify.Error = verr.Error()
		out.Verified = false
		out.OK = false
		out.Reason = fmt.Sprintf("verify: %v", verr)
	} else {
		out.Verified = true
	}
	out.Engine, out.Detail = engine, detail
	if err := backups.WriteManifest(dst, m); err != nil {
		// The artifact is usable; the sidecar is what makes it auditable.
		out.OK = false
		out.Reason = fmt.Sprintf("manifest: %v", err)
	}
	return out
}

// writeTarGz writes src into dst as a gzip'd tar whose members are
// "<member>/<rel>". It returns file count, byte count and the archive's
// SHA-256.
func writeTarGz(ctx context.Context, src, member, dst string, only []string) (files int, n int64, sha string, err error) {
	f, err := os.Create(dst)
	if err != nil {
		return 0, 0, "", fmt.Errorf("create %s: %w", filepath.Base(dst), err)
	}
	// The successful path closes the file explicitly (its error is the one
	// that matters); this deferred close is the cleanup for every early
	// return, so a double close must not be reported as the run's error.
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil && !errors.Is(cerr, os.ErrClosed) {
			err = cerr
		}
	}()
	h := sha256.New()
	mw := io.MultiWriter(f, h)
	gz := gzip.NewWriter(mw)
	tw := tar.NewWriter(gz)

	roots := []string{src}
	if len(only) > 0 {
		roots = nil
		for _, v := range only {
			roots = append(roots, filepath.Join(src, v))
		}
	}
	// base is the stack's PARENT directory: relative paths are computed from
	// there, which is what puts "<stack>/<volume>/…" after the member prefix
	// exactly once. Computing it from each root instead duplicated the stack
	// name ("backup/data/<stack>/<stack>/…") and would have restored every app
	// one directory too deep.
	base := filepath.Dir(src)
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return files, n, "", err
		}
		c, b, werr := addTree(ctx, tw, root, base, member)
		files += c
		n += b
		if werr != nil {
			return files, n, "", werr
		}
	}
	if err := tw.Close(); err != nil {
		return files, n, "", fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return files, n, "", fmt.Errorf("close gzip: %w", err)
	}
	if err := f.Close(); err != nil {
		return files, n, "", fmt.Errorf("close archive: %w", err)
	}
	return files, n, hex.EncodeToString(h.Sum(nil)), nil
}

// addTree walks root and writes it under member, with each member name computed
// relative to base. base is the stack's parent, so a member reads
// "backup/data/<stack>/<volume>/file" — the same shape the offen archives use,
// which is what browse, restore, --volume anchoring and prune-to-snapshot all
// expect.
func addTree(ctx context.Context, tw *tar.Writer, root, base, member string) (files int, n int64, err error) {
	return files, n, filepath.Walk(root, func(p string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		rel, rerr := filepath.Rel(base, p)
		if rerr != nil {
			return rerr
		}
		name := path.Join(member, filepath.ToSlash(rel))

		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, herr := tar.FileInfoHeader(info, link)
		if herr != nil {
			return herr
		}
		hdr.Name = name
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("header %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, oerr := os.Open(p)
		if oerr != nil {
			return oerr
		}
		defer file.Close()
		// archive/tar refuses to write past the size in the header, so a file
		// that grows mid-read fails the run instead of silently corrupting the
		// stream. That is the behaviour we want.
		written, cerr := io.Copy(tw, file)
		if cerr != nil {
			return fmt.Errorf("copy %s: %w", name, cerr)
		}
		files++
		n += written
		return nil
	})
}

// WritePlan and LoadPlan are the cached-plan pair the node uses so a run never
// blocks on the leader.
func WritePlan(p string, plan Plan) error {
	b, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// LoadPlan reads a cached plan.
func LoadPlan(p string) (Plan, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return Plan{}, err
	}
	var plan Plan
	if err := json.Unmarshal(b, &plan); err != nil {
		return Plan{}, fmt.Errorf("parse plan %s: %w", p, err)
	}
	return plan, nil
}

// WriteResult persists a run report so a failure to reach the leader can never
// lose the outcome. Files are kept newest-first by the caller's retention.
func WriteResult(dir string, res Result) (string, error) {
	if dir == "" {
		return "", nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, fmt.Sprintf("run-%d-%s.json", res.Started, sanitise(res.Node)))
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(p, append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

func sanitise(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, s)
}

// humanBytes formats a byte count for a log line.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
