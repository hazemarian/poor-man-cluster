package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backupagent"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
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
	agentShipWebDAV string
	agentDrillWorkD string
)

var backupAgentCmd = &cobra.Command{
	Use:   "backup-agent",
	Short: "Node-local snapshot agent (run by a systemd timer on storage nodes)",
	Long: `Take one consistent, per-stack backup of this node's volume root.

This is the host-side half of the backup pipeline. It runs on the node that
holds the data, freezes the filesystem for milliseconds, takes a single LVM
thin snapshot, and writes one archive per stack from the frozen image — so no
database is ever read while it is being written.

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
	Args: cobra.ExactArgs(1),
	RunE: runBackupAgentDrill,
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
	backupAgentRunCmd.Flags().IntVar(&agentKeepHours, "keep-hours", 48, "keep every artifact newer than this many hours (0 disables pruning)")
	backupAgentRunCmd.Flags().IntVar(&agentKeepDays, "keep-days", 15, "keep one artifact per day until this age in days")
	backupAgentRunCmd.Flags().IntVar(&agentKeepLast, "keep-last", 2, "never delete the newest N artifacts of a stack")
	backupAgentRunCmd.Flags().BoolVar(&agentKeepLegacy, "prune-legacy", false, "also prune whole-node archives (the previous producer's output)")
	backupAgentRunCmd.Flags().StringVar(&agentOnlyStack, "only-stack", "", "archive just this stack (operator or console asking for one stack)")
	backupAgentRunCmd.Flags().BoolVar(&agentForce, "force", false, "run now, ignoring the control plane's schedule decision")
	backupAgentRunCmd.Flags().BoolVar(&agentDryRun, "dry-run", false, "report what would be archived without freezing, snapshotting or writing")
	backupAgentRunCmd.Flags().StringVar(&agentShipWebDAV, "ship-webdav", envOr("PMCLUSTER_BACKUP_WEBDAV", ""),
		"WebDAV URL to copy each artifact to after verification (the in-cluster store)")
	backupAgentDrillCmd.Flags().StringVar(&agentDrillWorkD, "work-dir", "", "scratch directory (default: a temp dir)")
	backupAgentInstallCmd := &cobra.Command{
		Use:   "install",
		Short: "Install and enable the hourly root oneshot on this node",
		Long: `Install the systemd service + timer that run the agent every hour.

Needs root (the unit runs fsfreeze, lvcreate, mount). Opt-in rather than
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
	backupAgentCmd.AddCommand(backupAgentRunCmd, backupAgentDrillCmd, backupAgentInstallCmd)
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

	plan, planPath, err := loadAgentPlan(agentPlanPath, agentStateDir)
	if err != nil {
		// No published plan and no cache: derive one from the volume root. A
		// node whose control plane is unreachable must still be backed up, and
		// the directory layout already says what the stacks are.
		derived, derr := backupagent.PlanFromDisk(agentVolumeRoot)
		if derr != nil {
			return fmt.Errorf("load plan %s: %w (and deriving one from %s failed: %v)", planPath, err, agentVolumeRoot, derr)
		}
		plan = derived
		fmt.Fprintf(cmd.ErrOrStderr(), "note: no plan at %s — derived %d stack(s) from %s\n", err, len(plan.Stacks), agentVolumeRoot)
	}
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

	var retention *backups.Retention
	if agentKeepHours > 0 {
		policy := backups.DefaultRetention()
		policy.KeepHours = agentKeepHours
		policy.KeepDays = agentKeepDays
		policy.KeepLast = agentKeepLast
		policy.IncludeLegacy = agentKeepLegacy
		retention = &policy
	}
	a := &backupagent.Agent{
		Retention:        retention,
		Node:             node,
		VolumeRoot:       root,
		ArchiveDir:       agentArchiveDir,
		RunDir:           filepath.Join(agentStateDir, "runs"),
		MaxFreezeSeconds: maxFreezeSeconds(),
		Snapshot:         snap,
		DryRun:           agentDryRun,
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
	if errors.Is(runErr, backupagent.ErrBackingOff) {
		fmt.Fprintf(out, "%s\n", res.Error)
		return nil
	}
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
	if unplanned := backupagent.UnplannedDirs(root, plan); len(unplanned) > 0 {
		verb, aux := "are", "were"
		if len(unplanned) == 1 {
			verb, aux = "is", "was"
		}
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: %d director%s under %s %s not in the plan and %s NOT archived: %v\n",
			len(unplanned), plural(len(unplanned), "y", "ies"), root, verb, aux, unplanned)
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
	archive := args[0]
	if st, err := os.Stat(archive); err != nil {
		return fmt.Errorf("archive: %w", err)
	} else if st.IsDir() {
		return fmt.Errorf("%s is a directory, want a .tar.gz artifact", archive)
	}
	work := agentDrillWorkD
	if work == "" {
		var err error
		if work, err = os.MkdirTemp("", "pmcluster-drill-"); err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(work) }()
	}
	files, engine, detail, err := backupagent.Drill(archive, work)
	out := cmd.OutOrStdout()
	if err != nil {
		fmt.Fprintf(out, "✗ %s\n  %v\n", filepath.Base(archive), err)
		return err
	}
	fmt.Fprintf(out, "✓ %s\n  %d files extracted to %s\n  %s\n", filepath.Base(archive), files, work, detail)
	if engine == "" {
		fmt.Fprintf(out, "  note: no database found in this artifact — extraction is the whole drill\n")
	}
	return nil
}

// webdavShipper copies an artifact to the in-cluster store. It is a thin
// wrapper so the agent stays unaware of the transport.
// maxFreezeSeconds is the circuit-breaker limit: how long the previous run's
// freeze may have lasted before this run stands down for one interval.
//
// Thirty seconds is generous — a healthy node freezes for 100–600 ms — and
// anything above it means the node is not in a state where a snapshot is safe.
func maxFreezeSeconds() float64 {
	if v := envOr("PMCLUSTER_BACKUP_MAX_FREEZE_SECONDS", ""); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return 30
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
func shipTargetsFromEnv() func(context.Context, string) ([]string, error) {
	webdav := agentShipWebDAV
	s3 := backups.S3Config{
		Endpoint:  envOr("PMCLUSTER_BACKUP_S3_ENDPOINT", ""),
		Bucket:    envOr("PMCLUSTER_BACKUP_S3_BUCKET", ""),
		AccessKey: envOr("PMCLUSTER_BACKUP_S3_ACCESS_KEY", ""),
		SecretKey: envOr("PMCLUSTER_BACKUP_S3_SECRET_KEY", ""),
		Region:    envOr("PMCLUSTER_BACKUP_S3_REGION", ""),
	}
	// The node's policy file is authoritative when it exists: it is what the
	// installer wrote from the cluster settings.
	if pol, ok := readBackupPolicy(backupPolicyFile); ok {
		if pol.Store != "" && isOff(pol.Store) {
			webdav = ""
		}
		if pol.S3.Endpoint != "" {
			s3 = backups.S3Config{
				Endpoint:  pol.S3.Endpoint,
				Bucket:    pol.S3.Bucket,
				Region:    pol.S3.Region,
				AccessKey: pol.S3.AccessKey,
				SecretKey: pol.S3.SecretKey,
			}
		}
	}
	if webdav == "" && !s3.Configured() {
		return nil
	}
	return func(ctx context.Context, archivePath string) ([]string, error) {
		name := filepath.Base(archivePath)
		var where []string
		if webdav != "" {
			if err := backups.PutWebDAV(ctx, webdav, archivePath); err != nil {
				return where, err
			}
			where = append(where, "store:"+name)
		}
		if s3.Configured() {
			if err := backups.PutS3Object(ctx, s3, name, archivePath); err != nil {
				return where, err
			}
			where = append(where, "s3:"+s3.Bucket+"/"+name)
		}
		return where, nil
	}
}
