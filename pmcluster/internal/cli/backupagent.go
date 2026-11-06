package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backupagent"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
)

// Defaults for a host-side run. They match the paths the platform already
// uses, so an artifact written here is discovered by the existing listing
// code with no configuration at all.
const (
	defaultVolumeRoot = "/var/stack/data"
	defaultStateDir   = "/var/lib/pmcluster-backup"
	planCacheName     = "plan.json"
	agentRunDeadline  = 45 * time.Minute
)

var (
	agentForce      bool
	agentOnlyStack  string
	agentNode       string
	agentVolumeRoot string
	agentArchiveDir string
	agentPlanPath   string
	agentKeepHours  int
	agentKeepDays   int
	agentKeepLast   int
	agentKeepLegacy bool
	agentStateDir   string
	agentSnapshot   string
	agentVG         string
	agentLV         string
	agentSnapDir    string
	agentDryRun     bool
	agentDrillWorkD string
)

var backupAgentCmd = &cobra.Command{
	Use:   "backup-agent",
	Short: "Node-local snapshot agent (run by a systemd timer on storage nodes)",
	Long: `Take one consistent, per-stack backup of this node's volume root.

This is the host-side half of the backup pipeline. It runs on the node that
holds the data, takes a single LVM thin snapshot (a metadata operation, ~200 ms;
nothing is frozen), mounts the copy read-write so each filesystem's journal
replays, and writes one archive per stack from that copy — so no database is
ever read while it is being written, and every artifact is crash-consistent
rather than torn.

It is not a Swarm service on purpose: a task cannot hold the freeze/thaw
sequence safely, and it can be rescheduled mid-run. The unit that drives it is
installed by 'pmcluster cluster update'.

  pmcluster backup-agent run                     # one run, normally by the timer
  pmcluster backup-agent run --dry-run           # what would be captured
  pmcluster backup-agent drill <archive.tar.gz>  # prove an archive restores`,
}

var backupAgentRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Take one snapshot and write one archive per planned stack",
	RunE:  runBackupAgent,
}

var backupAgentDrillCmd = &cobra.Command{
	Use:   "drill <archive>",
	Short: "Extract an archive and verify the databases inside it",
	Long: `Restore drill: extract the artifact into a scratch directory and prove the
databases inside it open and answer an integrity check. This is the check that
makes "backed up" mean something; a listing alone proves nothing.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runBackupAgentDrill,
}

var backupAgentDrillStatusCmd = &cobra.Command{
	Use:   "drill-status",
	Short: "Show the last restore drill this node ran",
	Long: `Reads the record the drill leaves behind. The weekly timer writes it, so this
answers "has anything been extracted and verified lately?" without a journal grep.

A drill that has never run says so; that is a fact worth having, not an error.`,
	Args: cobra.NoArgs,
	RunE: runBackupAgentDrillStatus,
}

var agentDrillNewest int

// agentIncludeOrphans archives directories the plan calls orphans instead of
// skipping them.
var agentIncludeOrphans bool

// derivePlanLocally builds a plan without the control plane, in the order a node
// can actually trust: what its own containers use, then the directory listing.
//
// The container step is what a worker needs. `docker service ls` is refused on a
// worker, and its policy file carries no S3 credentials, so it can neither read
// the swarm nor fetch a published plan — but its own docker daemon knows which
// stacks have containers here and which host directories they mount. That is the
// live answer, and it needs no credential at all.
func derivePlanLocally(ctx context.Context, root, archiveDir string) (backupagent.Plan, string) {
	if dc, derr := docker.New(); derr == nil {
		defer func() { _ = dc.Close() }()
		if mounts, merr := dc.ContainerMounts(ctx); merr == nil && len(mounts) > 0 {
			if plan, perr := backupagent.PlanFromMounts(root, mounts, recentlyArchivedStacks(archiveDir), time.Now()); perr == nil {
				return plan, fmt.Sprintf("derived %d live stack(s) from this node's %d bind mount(s); %d director(y/ies) belong to no container",
					len(plan.Stacks), len(mounts), len(plan.Orphans))
			}
		}
	}
	plan, _ := backupagent.PlanFromDisk(root)
	return plan, fmt.Sprintf("derived %d stack(s) from %s (no container data on this node: every directory is archived)",
		len(plan.Stacks), root)
}

// recentlyArchivedStacks names the stacks with an artifact written inside the
// live window: a stack we were backing up an hour ago is not an orphan.
func recentlyArchivedStacks(dir string) map[string]bool {
	out := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	cutoff := time.Now().Add(-backupagent.LiveMtimeWindow)
	for _, e := range entries {
		if e.IsDir() || !backups.IsArtifact(e.Name()) {
			continue
		}
		fi, ferr := e.Info()
		if ferr != nil || fi.ModTime().Before(cutoff) {
			continue
		}
		if art, ok := backups.ParseArtifact(e.Name()); ok && art.Stack != "" {
			out[art.Stack] = true
		}
	}
	return out
}

// applyOrphanPolicy reconciles the published plan with what is on disk.
//
// A plan lists the stacks that exist, and the run archives exactly those —
// directories it does not cover are already skipped, and the run's own
// unplanned-directory warning names them. Archiving them forever is pure cost (87
// MB of one production node's hourly payload was volumes of stacks that no longer
// exist) and restoring one is meaningless.
//
// The one thing that must not happen is "we quietly stopped backing up a live
// stack", so a directory whose contents changed AFTER the plan was generated is
// adopted back into the plan: a stack deployed since then is writing there, and
// its data would otherwise be invisible until the next plan publish. A plan with
// no GeneratedAt (written by an older daemon) adopts nothing — the long-standing
// behaviour.
func applyOrphanPolicy(plan backupagent.Plan, root string, out io.Writer, includeOrphans bool) backupagent.Plan {
	orphans, err := backupagent.OrphanDirs(root, plan)
	if err != nil || len(orphans) == 0 {
		return plan
	}
	var adopted []string
	for _, dir := range orphans {
		active := plan.GeneratedAt > 0 && backupagent.NewestMtime(filepath.Join(root, dir)) > plan.GeneratedAt
		if !includeOrphans && !active {
			continue
		}
		plan.Stacks = append(plan.Stacks, backupagent.StackPlan{Name: dir})
		adopted = append(adopted, dir)
	}
	if len(adopted) > 0 {
		sort.Slice(plan.Stacks, func(i, j int) bool { return plan.Stacks[i].Name < plan.Stacks[j].Name })
		fmt.Fprintf(out, "note: %d unplanned director(y/ies) changed since the plan was published — archiving them anyway: %s\n",
			len(adopted), strings.Join(adopted, ", "))
	}
	return plan
}

func init() {
	backupAgentRunCmd.Flags().StringVar(&agentNode, "node", "", "swarm node id (default: $PMCLUSTER_NODE_ID, else the plan's node)")
	backupAgentRunCmd.Flags().StringVar(&agentVolumeRoot, "volume-root", envOr("PMCLUSTER_VOLUME_ROOT", defaultVolumeRoot), "host directory holding every stack's data")
	backupAgentRunCmd.Flags().StringVar(&agentArchiveDir, "archive-dir", envOr("PMCLUSTER_BACKUP_DIR", backups.DefaultArchiveDir), "where artifacts and their sidecars are written")
	backupAgentRunCmd.Flags().StringVar(&agentPlanPath, "plan", "", "plan file (default: <state-dir>/plan.json; when that is absent the plan is derived from the volume root)")
	backupAgentRunCmd.Flags().StringVar(&agentStateDir, "state-dir", envOr("PMCLUSTER_BACKUP_STATE", defaultStateDir), "agent state: cached plan, run records, lock")
	backupAgentRunCmd.Flags().StringVar(&agentSnapshot, "snapshot", "lvm", "consistency mechanism: lvm (production) or none (tests, hosts without LVM)")
	backupAgentRunCmd.Flags().StringVar(&agentVG, "vg", envOr("PMCLUSTER_BACKUP_VG", ""), "volume group holding the volume root (lvm only)")
	backupAgentRunCmd.Flags().StringVar(&agentLV, "lv", envOr("PMCLUSTER_BACKUP_LV", ""), "logical volume holding the volume root (lvm only)")
	backupAgentRunCmd.Flags().StringVar(&agentSnapDir, "snap-dir", "/run/pmcluster-backup/snap", "mount point for the read-only snapshot")
	backupAgentRunCmd.Flags().IntVar(&agentKeepHours, "keep-hours", 0, "override the dense window (default 48 hours; 0 keeps one per day from the start)")
	backupAgentRunCmd.Flags().IntVar(&agentKeepDays, "keep-days", 0, "override the sparse window (default: backup_retention_days, else 3 days)")
	backupAgentRunCmd.Flags().IntVar(&agentKeepLast, "keep-last", 2, "never delete the newest N artifacts of a stack")
	backupAgentRunCmd.Flags().BoolVar(&agentKeepLegacy, "prune-legacy", false, "also prune whole-node archives (the previous producer's output)")
	backupAgentRunCmd.Flags().StringVar(&agentOnlyStack, "only-stack", "", "archive just this stack (operator or console asking for one stack)")
	backupAgentRunCmd.Flags().BoolVar(&agentForce, "force", false, "run now, ignoring the control plane's schedule decision")
	backupAgentRunCmd.Flags().BoolVar(&agentIncludeOrphans, "include-orphans", false, "archive volume directories the plan does not cover (leftovers of removed stacks)")
	backupAgentRunCmd.Flags().BoolVar(&agentDryRun, "dry-run", false, "report what would be archived without freezing, snapshotting or writing")
	backupAgentDrillCmd.Flags().StringVar(&agentDrillWorkD, "work-dir", "", "scratch directory (default: a temp dir)")
	backupAgentDrillCmd.Flags().IntVar(&agentDrillNewest, "newest", 0, "drill the N newest artifacts in --archive-dir instead of naming one on the command line (the weekly timer uses 1)")
	backupAgentInstallCmd := &cobra.Command{
		Use:   "install",
		Short: "Install and enable the hourly root oneshot on this node",
		Long: `Install the systemd service + timer that run the agent every hour.

Needs root (the unit runs lvcreate, mount, umount). Opt-in rather than
automatic: the two producers write different archives, and a node must not run
both against the same data by accident — watch an hour of per-stack artifacts
before retiring that node's volume-backup agent.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if os.Geteuid() != 0 {
				return fmt.Errorf("installing the hourly unit needs root — re-run with sudo")
			}
			return ensureBackupAgentInstalled(cmd.OutOrStdout())
		},
	}
	backupAgentCmd.AddCommand(backupAgentRunCmd, backupAgentDrillCmd, backupAgentDrillStatusCmd,
		backupAgentVerifyOffsiteCmd, offsiteStatusCmd, backupAgentInstallCmd)
	rootCmd.AddCommand(backupAgentCmd)
}

// resolveAgentNode answers "which node am I?" for an artifact name.
//
// Order matters: an explicit flag, then the environment the systemd unit sets,
// then the swarm's own answer, then the hostname. The swarm branch is what makes
// a first run work on a node nobody configured — without it a derived plan and
// no PMCLUSTER_NODE_ID is a hard failure, which is exactly the state a freshly
// provisioned node is in on the day the agent is installed.
func resolveAgentNode(ctx context.Context, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("PMCLUSTER_NODE_ID"); v != "" {
		return v
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.Swarm.NodeID}}").Output(); err == nil {
		if id := strings.TrimSpace(string(out)); id != "" {
			return id
		}
	}
	if hn, err := os.Hostname(); err == nil {
		return hn
	}
	return ""
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// loadAgentPlan prefers the cached copy: a run must never depend on the leader
// being reachable. --plan and the cache hold the same JSON, so an operator can
// hand-place a plan for a first run.
func loadAgentPlan(explicit, stateDir string) (backupagent.Plan, string, error) {
	p := explicit
	if p == "" {
		p = filepath.Join(stateDir, planCacheName)
	}
	plan, err := backupagent.LoadPlan(p)
	if err != nil {
		return backupagent.Plan{}, p, err
	}
	return plan, p, nil
}

func runBackupAgent(cmd *cobra.Command, _ []string) error {
	cmd.SilenceUsage = true
	// Spans are only useful if a provider is installed: without this the tracer
	// is the global no-op and a slow phase leaves no trace behind.
	defer initCLITelemetry()()
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out := cmd.OutOrStdout()

	// A run may not outlive the timer interval: an unbounded phase would hold the
	// flock past the next trigger, which exits silently as "already running" and
	// turns a hung node into a node with no backups and no error.
	// Derived from the command's context, not a fresh one: a SIGTERM that
	// cancels the command must also cancel the run.
	ctx, cancel := context.WithTimeout(ctx, agentRunDeadline)
	defer cancel()

	plan, _, err := loadAgentPlan(agentPlanPath, agentStateDir)
	if err != nil {
		// No published plan and no cache. Both remaining derivations are local —
		// nothing is fetched from a peer, so a node whose control plane is
		// unreachable still backs up.
		derived, how := derivePlanLocally(ctx, agentVolumeRoot, agentArchiveDir)
		plan = derived
		fmt.Fprintf(cmd.ErrOrStderr(), "note: no plan at %s — %s\n", err, how)
	} else {
		plan = applyOrphanPolicy(plan, agentVolumeRoot, out, agentIncludeOrphans)
	}
	// The plan's own list, kept before --only-stack can narrow it: the
	// unplanned-directory warning is about what the PLAN covers, not about what
	// this particular run was asked for.
	plannedNames := plan.StackNames()

	node := resolveAgentNode(ctx, agentNode)
	if node == "" {
		node = plan.Node
	}
	if node == "" {
		return fmt.Errorf("no node id: set --node or PMCLUSTER_NODE_ID, or give the plan a node")
	}
	root := agentVolumeRoot
	if plan.VolumeRoot != "" && agentVolumeRoot == defaultVolumeRoot {
		root = plan.VolumeRoot
	}

	var snap backupagent.Snapshotter
	switch agentSnapshot {
	case "lvm":
		snap = &backupagent.LVMSnapshotter{Root: root, VG: agentVG, LV: agentLV, SnapDir: agentSnapDir}
	case "none":
		// Explicitly requested: no freeze, no snapshot. Correct for tests and
		// hosts without LVM; never the default, because a tar of a live tree
		// is the corruption this agent exists to prevent.
		snap = &backupagent.NoSnapshot{Root: root}
	default:
		return fmt.Errorf("unknown --snapshot %q (want lvm or none)", agentSnapshot)
	}

	// Retention comes from the node's policy file (which carries
	// backup_retention_days), falling back to the setting and then to the 48h/3d
	// default. A flag overrides it only when the operator actually passed one:
	// deciding on the flag VALUES let their defaults overwrite the cluster
	// setting, so a cluster configured for three days pruned at fifteen and the
	// setting read as decoration.
	policy := localRetentionPolicy(ctx)
	flags := cmd.Flags()
	if flags.Changed("keep-hours") {
		policy.KeepHours = agentKeepHours
	}
	if flags.Changed("keep-days") {
		policy.KeepDays = agentKeepDays
	}
	if flags.Changed("keep-last") {
		policy.KeepLast = agentKeepLast
	}
	if flags.Changed("prune-legacy") {
		policy.IncludeLegacy = agentKeepLegacy
	}
	retention := &policy
	a := &backupagent.Agent{
		Retention:    retention,
		KeepLocalOne: keepLocalMinimal(),
		Node:         node,
		VolumeRoot:   root,
		ArchiveDir:   agentArchiveDir,
		Snapshot:     snap,
		DryRun:       agentDryRun,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", args...)
		},
	}
	// One stack on request: the operator asked for THIS stack, so the run is
	// narrowed before anything is frozen. An unknown name is an error, not an
	// empty run — silence would look like success.
	if agentOnlyStack != "" {
		var kept []backupagent.StackPlan
		for _, sp := range plan.Stacks {
			if sp.Name == agentOnlyStack {
				kept = append(kept, sp)
			}
		}
		if len(kept) == 0 {
			return fmt.Errorf("stack %q is not planned on this node (planned: %s)", agentOnlyStack, strings.Join(plan.StackNames(), ", "))
		}
		plan.Stacks = kept
	}

	// The control plane decides whether this hour is due. The systemd timer is a
	// dumb heartbeat: it fires every hour and this is where a run is actually
	// authorised. --force exists for an operator or the console asking for a run
	// now, and is the only way past the gate.
	if !agentForce && !agentDryRun {
		dec, derr := localBackupDecision(ctx)
		switch {
		case derr != nil:
			// No settings on this node. The decision already says "due", so this
			// is a notice, not a gate: a node the platform has not configured
			// still backs up.
			fmt.Fprintf(out, "backup running with defaults: no local settings (%v)\n", derr)
		case !dec.Enabled:
			fmt.Fprintf(out, "backup skipped: %s\n", dec.Reason)
			return nil
		case !dec.Due:
			fmt.Fprintf(out, "backup not due: %s\n", dec.Reason)
			return nil
		default:
			fmt.Fprintf(out, "backup authorised by the control plane (schedule %s, store=%t, offsite=%t)\n", dec.Schedule, dec.Store, dec.Offsite)
		}
	}

	if ship := shipTargetsFromEnv(); ship != nil {
		a.Ship = ship
	}

	res, runErr := a.Run(ctx, plan)
	if errors.Is(runErr, backupagent.ErrAlreadyRunning) {
		// Somebody else is already backing this node up. Not an error: the work
		// is being done by the run that holds the lock.
		fmt.Fprintln(out, "another backup run is in progress on this node — nothing to do")
		return nil
	}

	// Report the run before anything else can fail: a node that cannot reach
	// the leader still has to leave an audit trail behind.
	if p, err := backupagent.WriteResult(filepath.Join(agentStateDir, "runs"), res); err == nil && p != "" {
		fmt.Fprintf(out, "run record: %s\n", p)
	}
	printAgentResult(out, res)
	if unplanned := backupagent.UnplannedDirsFor(root, plannedNames); len(unplanned) > 0 {
		verb, aux := "are", "were"
		if len(unplanned) == 1 {
			verb, aux = "is", "was"
		}
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: %d director%s under %s %s not in the plan and %s NOT archived: %v\n",
			len(unplanned), plural(len(unplanned), "y", "ies"), root, verb, aux, unplanned)
	}
	// The store and the bucket are pruned on the same policy as the node. Nothing
	// used to delete from either: an upload loop with no delete grows without
	// limit until the bucket is full and every upload starts failing.
	if !agentDryRun {
		pruneRemoteLegs(ctx, out, policy)
	}
	if runErr != nil {
		return runErr
	}
	if !res.Succeeded() {
		return fmt.Errorf("backup-agent: %d of %d stacks failed", failedCount(res), len(res.Stacks))
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func failedCount(r backupagent.Result) int {
	n := 0
	for _, s := range r.Stacks {
		if !s.OK {
			n++
		}
	}
	return n
}

func printAgentResult(out interface{ Write([]byte) (int, error) }, res backupagent.Result) {
	for _, s := range res.Stacks {
		switch {
		case s.Empty:
			fmt.Fprintf(out, "○ %-24s nothing to archive — %s\n", s.Stack, s.Detail)
		case s.OK && s.Verified:
			fmt.Fprintf(out, "✓ %-24s %8s  %s  [verified %s]\n", s.Stack, humanBytes(s.Bytes), s.Archive, s.Engine)
		case s.OK:
			fmt.Fprintf(out, "· %-24s %8s  %s  [unverified: %s]\n", s.Stack, humanBytes(s.Bytes), s.Archive, s.Detail)
		default:
			fmt.Fprintf(out, "✗ %-24s %s\n", s.Stack, s.Reason)
		}
	}
	freeze := time.Duration(0)
	if res.Snapshot.ThawedAt > res.Snapshot.FrozenAt && res.Snapshot.FrozenAt > 0 {
		freeze = time.Duration(res.Snapshot.ThawedAt - res.Snapshot.FrozenAt)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
	fmt.Fprintf(out, "\nnode %s · %d stack(s) · snapshot took %s · snapshot %s",
		res.Node, len(res.Stacks), freeze, res.Snapshot.Kind)
	if res.Snapshot.PoolPercent >= 0 && res.Snapshot.PoolPercent != 0 {
		fmt.Fprintf(out, " · pool %.1f%%", res.Snapshot.PoolPercent)
	}
	fmt.Fprintln(out)
	if res.Error != "" {
		fmt.Fprintf(out, "run failed: %s\n", res.Error)
	}
}

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
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func runBackupAgentDrill(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	out := cmd.OutOrStdout()

	var archives []string
	switch {
	case agentDrillNewest > 0:
		if len(args) > 0 {
			return fmt.Errorf("--newest picks the artifacts itself; do not also name one on the command line")
		}
		found, err := newestArtifacts(agentArchiveDir, agentDrillNewest)
		if err != nil {
			return fmt.Errorf("find artifacts in %s: %w", agentArchiveDir, err)
		}
		if len(found) == 0 {
			// An empty archive dir is a real answer, not a failure: a node with no
			// artifacts has nothing to drill, and the weekly timer must not report
			// that as a broken drill.
			fmt.Fprintf(out, "no artifacts in %s — nothing to drill\n", agentArchiveDir)
			return nil
		}
		archives = found
	case len(args) == 1:
		archives = args
	default:
		return fmt.Errorf("name an archive, or pass --newest N to drill the newest ones in %s", agentArchiveDir)
	}

	var failures int
	for _, archive := range archives {
		rec := drillRecord{At: time.Now().UTC(), Archive: filepath.Base(archive)}
		fi, statErr := os.Stat(archive)
		if statErr != nil {
			rec.Error = statErr.Error()
			_ = recordDrill(agentStatedirForDrill(), rec)
			fmt.Fprintf(out, "✗ %s\n  %v\n", rec.Archive, statErr)
			failures++
			continue
		}
		if fi.IsDir() {
			return fmt.Errorf("%s is a directory, want a .tar.gz artifact", archive)
		}
		work := agentDrillWorkD
		if work == "" {
			work = os.TempDir()
		}
		if err := ensureDrillSpace(work, fi.Size()); err != nil {
			rec.Error = err.Error()
			_ = recordDrill(agentStatedirForDrill(), rec)
			fmt.Fprintf(out, "✗ %s\n  %v\n", rec.Archive, err)
			failures++
			continue
		}
		scratch, err := os.MkdirTemp(work, "pmcluster-drill-")
		if err != nil {
			return err
		}
		files, engine, detail, derr := backupagent.Drill(archive, scratch)
		_ = os.RemoveAll(scratch)
		rec.Files, rec.Engine, rec.Detail = files, engine, detail
		if derr != nil {
			rec.Error = derr.Error()
			_ = recordDrill(agentStatedirForDrill(), rec)
			fmt.Fprintf(out, "✗ %s\n  %v\n", rec.Archive, derr)
			failures++
			continue
		}
		rec.OK = true
		_ = recordDrill(agentStatedirForDrill(), rec)
		fmt.Fprintf(out, "✓ %s\n  %d files extracted\n  %s\n", rec.Archive, files, detail)
		if engine == "" {
			fmt.Fprintf(out, "  note: no database found in this artifact — extraction is the whole drill\n")
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d drill(s) failed", failures, len(archives))
	}
	return nil
}

// agentStatedirForDrill is the state dir the drill records into. It honours the
// same override the agent uses, so a test (or an operator with a custom state
// dir) does not write into /var/lib.
func agentStatedirForDrill() string {
	if agentStateDir != "" {
		return agentStateDir
	}
	return defaultStateDir
}

func runBackupAgentDrillStatus(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	p := drillStatusPath(agentStatedirForDrill())
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(out, "no drill has run on this node yet (%s does not exist)\n", p)
			fmt.Fprintln(out, "  the weekly timer installs with `pmcluster backup-agent install`; run one now with")
			fmt.Fprintln(out, "  `pmcluster backup-agent drill --newest 1`")
			return nil
		}
		return err
	}
	var doc drillStatusFile
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", p, err)
	}
	if doc.Last == nil {
		fmt.Fprintf(out, "no drill recorded in %s\n", p)
		return nil
	}
	last := doc.Last
	mark := "✗"
	if last.OK {
		mark = "✓"
	}
	fmt.Fprintf(out, "%s last drill: %s\n", mark, last.At.Format(time.RFC3339))
	fmt.Fprintf(out, "  archive: %s\n  files:   %d\n", last.Archive, last.Files)
	if last.Detail != "" {
		fmt.Fprintf(out, "  detail:  %s\n", last.Detail)
	}
	if last.Error != "" {
		fmt.Fprintf(out, "  error:   %s\n", last.Error)
	}
	if len(doc.History) > 1 {
		fmt.Fprint(out, "\nrecent drills:\n")
		for i, h := range doc.History {
			if i >= 5 {
				break
			}
			m := "✗"
			if h.OK {
				m = "✓"
			}
			fmt.Fprintf(out, "  %s %s  %s\n", m, h.At.Format("2006-01-02 15:04"), h.Archive)
		}
	}
	return nil
}

// localBackupDecision answers "should this hour run?" from the node's own copy
// of the control-plane store.
//
// There is no endpoint and no cross-node call: every node already holds the
// settings in a volume (~/.pmcluster/data.db), restored from the control-plane
// archive, so the agent reads them the same way the CLI does. That keeps the
// schedule in the control plane's hands without opening a port, minting a token,
// or putting a bucket key on the wire.
//
// The copy is as fresh as the node's last restore (the control-plane archive runs
// daily, and a manager refreshes it on promotion), which is the right latency for
// a schedule setting and the wrong latency for an emergency stop — for that,
// stop the unit on the node.
func localBackupDecision(ctx context.Context) (cluster.BackupDecision, error) {
	if pol, ok := readBackupPolicy(backupPolicyFile); ok {
		return decisionFromPolicy(pol, time.Now()), nil
	}
	st, _, err := openStore()
	if err != nil {
		// No policy file and no store: a node the platform has not configured.
		// It must still back up — an unconfigured node is not an exempt one —
		// so the defaults are enabled and due, not a zero decision that would
		// read as "not due".
		return cluster.BackupDecision{
			Enabled:  true,
			Due:      true,
			Schedule: cluster.DefaultBackupCron,
			Reason:   "no local settings; running with the defaults",
		}, err
	}
	defer func() { _ = st.Close() }()
	return cluster.BackupDecisionFor(ctx, st, time.Now()), nil
}

// shipTargetsFromEnv builds// shipTargetsFromEnv builds the artifact's destinations from the environment.
//
// Both legs are optional and independent: the in-cluster store (WebDAV) is one
// way to keep every backup in a single place, the offsite bucket is another, and
// a node with neither is a legitimate configuration — the artifact still lands
// on disk and the run records where. Returning nil means "local only", which the
// agent reports as such rather than as a failure.
// shipTarget is one destination for an artifact. Store and bucket are the same
// thing: an S3 endpoint, a bucket and a credential. Keeping them as separate
// mechanisms (WebDAV for one, S3 for the other) meant two upload paths, two
// retention implementations and two sets of timeouts to get wrong.
type shipTarget struct {
	// Label is what the run record calls this destination ("store" or "s3").
	Label string
	S3    backups.S3Config
}

// backupShipTargets resolves where artifacts go.
//
// The in-cluster store is one target, the offsite bucket is another, and a node
// with neither is a legitimate configuration — the artifact still lands on disk
// and the run records where. The node's policy file wins over the environment: it
// is what the installer wrote from the cluster's own settings.
func backupShipTargets() []shipTarget {
	var targets []shipTarget

	pol, havePol := readBackupPolicy(backupPolicyFile)
	storeOff := havePol && pol.Store != "" && isOff(pol.Store)

	// The in-cluster store, as an S3 target in its own right.
	if !storeOff {
		store := backups.S3Config{
			Endpoint:  envOr("PMCLUSTER_BACKUP_STORE_S3_ENDPOINT", ""),
			Bucket:    envOr("PMCLUSTER_BACKUP_STORE_S3_BUCKET", ""),
			AccessKey: envOr("PMCLUSTER_BACKUP_STORE_S3_ACCESS_KEY", ""),
			SecretKey: envOr("PMCLUSTER_BACKUP_STORE_S3_SECRET_KEY", ""),
			Region:    envOr("PMCLUSTER_BACKUP_STORE_S3_REGION", ""),
		}
		if havePol && pol.StoreS3.Endpoint != "" {
			store = pol.StoreS3.config()
		}
		if store.Configured() {
			targets = append(targets, shipTarget{Label: "store", S3: store})
		}
	}

	// The offsite bucket.
	s3 := backups.S3Config{
		Endpoint:  envOr("PMCLUSTER_BACKUP_S3_ENDPOINT", ""),
		Bucket:    envOr("PMCLUSTER_BACKUP_S3_BUCKET", ""),
		AccessKey: envOr("PMCLUSTER_BACKUP_S3_ACCESS_KEY", ""),
		SecretKey: envOr("PMCLUSTER_BACKUP_S3_SECRET_KEY", ""),
		Region:    envOr("PMCLUSTER_BACKUP_S3_REGION", ""),
	}
	if havePol && pol.S3.Endpoint != "" {
		s3 = pol.S3.config()
	}
	if s3.Configured() {
		targets = append(targets, shipTarget{Label: "s3", S3: s3})
	}
	return targets
}

// shipTargetsFromEnv returns the sink the agent calls after an artifact is
// verified, or nil when the artifact stays on the node.
func shipTargetsFromEnv() func(context.Context, string) ([]string, error) {
	targets := backupShipTargets()
	if len(targets) == 0 {
		return nil
	}
	return func(ctx context.Context, archivePath string) ([]string, error) {
		name := filepath.Base(archivePath)
		manifest := backups.ManifestName(archivePath)
		hasManifest := false
		if _, err := os.Stat(manifest); err == nil {
			hasManifest = true
		}
		var where []string
		for _, t := range targets {
			// The sidecar goes FIRST. It carries the sha256, the snapshot facts and
			// the verification result: without it an offsite copy is a tarball
			// nobody can prove is the one that was taken. Uploading it first means
			// a failed second call leaves an orphan manifest (which pruning
			// removes) rather than an archive whose provenance is lost.
			if hasManifest {
				if err := backups.PutS3Object(ctx, t.S3, backups.ManifestName(name), manifest); err != nil {
					return where, fmt.Errorf("%s manifest: %w", t.Label, err)
				}
			}
			if err := backups.PutS3Object(ctx, t.S3, name, archivePath); err != nil {
				return where, fmt.Errorf("%s: %w", t.Label, err)
			}
			// A 2xx is not persistence. This store has answered 200 for
			// artifacts that were gone from the bucket hours later, while the
			// run recorded "store:..." as a destination and nothing looked
			// again until a restore. Verifying the object by name turns "we
			// sent it" into "it is there" at the moment it fails.
			ok, verr := backups.S3ObjectExists(ctx, t.S3, name)
			if verr != nil {
				return where, fmt.Errorf("%s: upload not confirmed: %w", t.Label, verr)
			}
			if !ok {
				return where, fmt.Errorf("%s: uploaded %s but the object is not in bucket %s — the target accepted the write and did not keep it", t.Label, name, t.S3.Bucket)
			}
			where = append(where, t.Label+":"+t.S3.Bucket+"/"+name)
		}
		return where, nil
	}
}

// pruneRemoteLegs applies retention to every configured destination.
//
// Retention used to run only on the node, so the store and the bucket were written
// to forever. Failures are reported, never fatal: a store that is down must not turn
// a successful backup into a failed one, but it must not pass silently for months
// either.
func pruneRemoteLegs(ctx context.Context, out io.Writer, policy backups.Retention) {
	now := time.Now()
	for _, t := range backupShipTargets() {
		res, err := backups.PruneS3(ctx, t.S3, policy, now, false)
		switch {
		case err != nil:
			fmt.Fprintf(out, "%s retention skipped: %v\n", t.Label, err)
		case len(res.Deleted) > 0 || len(res.Failed) > 0:
			fmt.Fprintf(out, "%s retention: deleted %d, kept %d, failed %d\n",
				t.Label, len(res.Deleted), res.Kept, len(res.Failed))
		}
		// The control-plane family has its own producer and its own age-out.
		// Nothing pruned it, so it grew without bound (40 GB on the live
		// store, on a node with 6 GB free).
		if del, kept, cerr := backups.PruneControlPlaneArchives(ctx, t.S3, now, false); cerr != nil {
			fmt.Fprintf(out, "%s control-plane retention skipped: %v\n", t.Label, cerr)
		} else if del > 0 {
			fmt.Fprintf(out, "%s control-plane retention: deleted %d, kept %d\n", t.Label, del, kept)
		}
	}
}
