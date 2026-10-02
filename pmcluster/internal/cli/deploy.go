package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/logger"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

var deployCmd = &cobra.Command{
	Use:   "deploy <manifest.yaml>",
	Short: "Deploy or update a stack from a DSL manifest file",
	Long: `Reads a pmcluster DSL manifest from disk, parses → interpolates →
validates → translates to Docker Swarm Compose, records a new revision
in SQLite, and applies it via 'docker stack deploy'.

Idempotent: re-running produces a new revision and re-applies (Docker
reconciles in place). Each deploy gets a unix-timestamp revision id;
rollback re-applies a stored one.

Must run on the manager (needs docker.sock + the pmcluster data dir).
For remote deploys, hit the HTTP API directly.`,
	Args: cobra.ExactArgs(1),
	RunE: runDeploy,
}

var stackCmd = &cobra.Command{
	Use:   "stack",
	Short: "Inspect deployed stacks and their revisions",
}

var stackListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all stacks (name, current revision, last update)",
	RunE:  runStackList,
}

var stackShowCmd = &cobra.Command{
	Use:   "show <stack-name>",
	Short: "Show a stack's metadata and recent revisions",
	Args:  cobra.ExactArgs(1),
	RunE:  runStackShow,
}

var stackBadgeCmd = &cobra.Command{
	Use:   "badge <stack-name>",
	Short: "Print the README status-badge markdown for a stack",
	Long: `Prints a markdown image line pointing at the stack's public status
badge (healthy / in progress / degraded / error), e.g.:

  ![donation-campaign status](https://pmcluster.example.com/api/public/badge/donation-campaign)

With --services, prints the combined badge that shows the stack's main
health followed by one segment per service:

  ![donation-campaign services](https://pmcluster.example.com/api/public/badge/donation-campaign/services)

Paste the line into any GitHub README. The badge endpoint is public and
returns a small SVG derived from live swarm replica state + the stack's
recorded deploy errors.`,
	Args: cobra.ExactArgs(1),
	RunE: runStackBadge,
}

var stackMoveCmd = &cobra.Command{
	Use:   "move <stack-name>",
	Short: "Move a stateful stack's storage to another node",
	Long: `Triggers an on-demand backup of the whole volume root, restores the stack's
<VolumeRoot>/<stack> subtree on the target node (locally when the target is
this host, else via a one-shot swarm mover service that pulls the archive
from this host), pins the stack to the target (stack_pin_<stack>), and
re-deploys so the placement constraint is rendered.

The reconcile loop never moves the stack back: the per-stack pin outranks
the storage_nodes round-robin and the platform_node fallback.

Must run on a swarm manager with docker.sock + the pmcluster data dir.`,
	Args: cobra.ExactArgs(1),
	RunE: runStackMove,
}

var rollbackCmd = &cobra.Command{
	Use:   "rollback <stack-name> <revision>",
	Short: "Re-apply a stored revision as a new revision",
	Long:  `Re-applies the stored rendered YAML as a NEW revision so both deploys are recorded in the audit trail.`,
	Args:  cobra.ExactArgs(2),
	RunE:  runRollback,
}

func init() {
	deployCmd.Flags().String("app", "", "override the manifest's app name (multi-tenant deploys)")
	deployCmd.Flags().String("repo", "", "repository URL (audit metadata only — not fetched)")
	deployCmd.Flags().String("file", "", "manifest path inside the source repo (provenance, e.g. deploy/test-lms.yaml)")
	deployCmd.Flags().String("version", "", "override the manifest's version (image tag)")

	stackCmd.AddCommand(stackListCmd, stackShowCmd, stackBadgeCmd, stackMoveCmd)
	stackBadgeCmd.Flags().Bool("services", false, "print the combined badge: stack main health + one segment per service")
	stackMoveCmd.Flags().String("to", "", "target node hostname to move the stack's storage to")
	_ = stackMoveCmd.MarkFlagRequired("to")

	rootCmd.AddCommand(deployCmd, stackCmd, rollbackCmd)
}

// openDeploySvc shares the boilerplate across the deploy/stack commands.
// Caller MUST defer the returned closer.
func openDeploySvc(cmd *cobra.Command) (*stacks.Service, *store.Store, func(), error) {
	st, cfg, err := openStore()
	if err != nil {
		return nil, nil, nil, err
	}
	// Local CLI deploys get a console logger honouring the configured
	// log_level, so the ordered-level markers + per-level compose YAML are
	// visible exactly when log_level=debug (and flow to OpenObserve via the
	// logger's OTel writer). Errors here are non-fatal — a missing logger
	// just means no structured deploy diagnostics.
	log, _, logErr := logger.New(logger.Options{Console: true, Level: cfg.LogLevel, ConsoleOut: cmd.OutOrStdout()})
	if logErr != nil {
		log = zerolog.Nop()
	}
	deployer := cluster.NewDockerCLIDeployer(cmd.OutOrStdout())
	svc := &stacks.Service{Store: st, Deployer: deployer, Backup: backups.LocalTrigger{Store: st}, Resolver: &stacks.StoreConfigResolver{Store: st}, VolumeRoot: st.GetSettingDefault(context.Background(), cluster.SettingVolumeRoot(), ""), CertResolver: cluster.CertResolverForMode(st.GetSettingDefault(context.Background(), cluster.SettingTLSMode(), "")), PinNode: st.GetSettingDefault(context.Background(), cluster.SettingPlatformNode(), ""), Pins: &stacks.PinResolver{PlatformNode: st.GetSettingDefault(context.Background(), cluster.SettingPlatformNode(), ""), StorageNodes: stacks.ParseStorageNodes(st.GetSettingDefault(context.Background(), cluster.SettingStorageNodes(), "")), StackPin: func(ctx context.Context, stackName string) (string, error) {
		return st.GetSettingDefault(ctx, stacks.StackPinKey(stackName), ""), nil
	}}, Stdout: cmd.OutOrStdout(), Log: log, BackupDir: cluster.BackupRootDir()}
	return svc, st, func() { _ = st.Close() }, nil
}

func runDeploy(cmd *cobra.Command, args []string) error {
	defer initCLITelemetry()()

	manifestPath := args[0]
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest %s: %w", manifestPath, err)
	}

	appOverride, _ := cmd.Flags().GetString("app")
	repo, _ := cmd.Flags().GetString("repo")
	file, _ := cmd.Flags().GetString("file")
	version, _ := cmd.Flags().GetString("version")
	payload := stacks.Payload{
		AppName:  appOverride,
		RepoURL:  repo,
		File:     file,
		Version:  version,
		Manifest: string(manifestBytes),
	}

	deployer, closeFn, err := backendDeploy(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	res, err := deployer.Deploy(cmd.Context(), payload)
	if err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(),
		"\n✅ Deployed %s @ revision %d (%s)\n",
		res.StackName, res.Revision, time.Unix(res.Revision, 0).Format(time.RFC3339),
	)
	return nil
}

func runStackList(cmd *cobra.Command, _ []string) error {
	reader, closeFn, err := backendStacks(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	list, err := reader.List(cmd.Context())
	if err != nil {
		return fmt.Errorf("list stacks: %w", err)
	}
	if len(list) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no stacks yet — use `pmcluster deploy <manifest.yaml>`)")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tREVISION\tUPDATED\tREPO\tFILE")
	for _, s := range list {
		repo := "—"
		if s.RepoURL != "" {
			repo = s.RepoURL
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n",
			s.Name, s.CurrentRevision,
			time.Unix(s.UpdatedAt, 0).Format(time.RFC3339), repo, s.SourceFile,
		)
	}
	return w.Flush()
}

func runStackShow(cmd *cobra.Command, args []string) error {
	name := args[0]

	reader, closeFn, err := backendStacks(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	s, err := reader.Get(cmd.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrStackNotFound) {
			return fmt.Errorf("stack %q not found", name)
		}
		return err
	}
	revs, err := reader.Revisions(cmd.Context(), name, 20)
	if err != nil {
		return fmt.Errorf("list revisions: %w", err)
	}

	lastBackup := ""
	if bsvc, bclose, berr := backendBackups(cmd); berr == nil {
		defer bclose()
		if rows, lerr := bsvc.ListForStack(cmd.Context(), name); lerr == nil && len(rows) > 0 {
			lastBackup = formatLastBackupRow(rows[0])
		}
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Stack:            %s\n", s.Name)
	fmt.Fprintf(out, "Current revision: %d (%s)\n", s.CurrentRevision, time.Unix(s.CurrentRevision, 0).Format(time.RFC3339))
	if s.RepoURL != "" {
		fmt.Fprintf(out, "Repo:             %s\n", s.RepoURL)
	}
	if s.SourceFile != "" {
		fmt.Fprintf(out, "File:             %s\n", s.SourceFile)
	}
	fmt.Fprintf(out, "Created:          %s\n", time.Unix(s.CreatedAt, 0).Format(time.RFC3339))
	fmt.Fprintf(out, "Updated:          %s\n", time.Unix(s.UpdatedAt, 0).Format(time.RFC3339))
	if lastBackup == "" {
		lastBackup = "(none recorded)"
	}
	fmt.Fprintf(out, "Last backup:      %s\n", lastBackup)
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Recent revisions (%d):\n", len(revs))

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  REVISION\tCREATED")
	for _, r := range revs {
		marker := "  "
		if r.Revision == s.CurrentRevision {
			marker = "→ "
		}
		fmt.Fprintf(w, "%s%d\t%s\n", marker, r.Revision, time.Unix(r.CreatedAt, 0).Format(time.RFC3339))
	}
	return w.Flush()
}

// runStackBadge prints the markdown image line for a stack's public status
// badge. The base URL is the cluster's pmcluster.<domain> origin in local
// mode, or the configured API URL origin in remote mode.
func runStackBadge(cmd *cobra.Command, args []string) error {
	name := args[0]

	base := badgeBaseURL(cmd)
	if base == "" {
		return fmt.Errorf("cannot derive the public badge base URL — set PMCLUSTER_API_URL (remote) or run on a cluster with a configured domain")
	}

	services, _ := cmd.Flags().GetBool("services")
	path := "/api/public/badge/" + name
	alt := name + " status"
	if services {
		path += "/services"
		alt = name + " services"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "![%s](%s%s)\n", alt, base, path)
	return nil
}

// badgeBaseURL resolves the public origin used by README badges: the
// configured API URL in remote mode (origin, no /api suffix), otherwise the
// cluster's pmcluster.<domain> origin from the stored domain setting.
func badgeBaseURL(cmd *cobra.Command) string {
	if apiURL != "" {
		origin := apiURL
		origin = strings.TrimSuffix(origin, "/")
		origin = strings.TrimSuffix(origin, "/api")
		return origin
	}
	st, _, err := openStore()
	if err != nil {
		return ""
	}
	defer func() { _ = st.Close() }()
	domain := st.GetSettingDefault(cmd.Context(), cluster.SettingDomain(), "")
	if domain == "" {
		return ""
	}
	return "https://pmcluster." + domain
}

func formatLastBackupRow(b backups.Run) string {
	ts := time.Unix(b.StartedAt, 0).Format(time.RFC3339)
	revPart := ""
	if b.Revision != 0 {
		revPart = fmt.Sprintf(" (rev %d)", b.Revision)
	}
	errPart := ""
	if b.Status == "failed" && b.ErrorMessage != "" {
		errPart = " — " + b.ErrorMessage
	}
	return fmt.Sprintf("%s @ %s%s%s", b.Status, ts, revPart, errPart)
}

func runRollback(cmd *cobra.Command, args []string) error {
	defer initCLITelemetry()()

	name := args[0]
	rev, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return fmt.Errorf("revision must be an integer: %w", err)
	}

	deployer, closeFn, err := backendDeploy(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	res, err := deployer.Rollback(cmd.Context(), name, rev)
	if err != nil {
		if errors.Is(err, store.ErrRevisionNotFound) {
			return fmt.Errorf("revision %d not found for stack %q", rev, name)
		}
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"\n✅ Rolled back %s to revision %d (new revision %d, %s)\n",
		res.StackName, rev, res.Revision, time.Unix(res.Revision, 0).Format(time.RFC3339),
	)
	return nil
}

func runStackMove(cmd *cobra.Command, args []string) error {
	defer initCLITelemetry()()

	to, _ := cmd.Flags().GetString("to")

	svc, _, closeFn, err := openDeploySvc(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	// Node validation + mover transit need a live docker client; when the
	// daemon is unreachable the move falls back to a local restore (the
	// target must then be this host).
	if dc, derr := docker.New(); derr == nil {
		svc.Docker = dc
		defer func() { _ = dc.Close() }()
	}

	if err := svc.Move(cmd.Context(), args[0], to); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"\n✅ Moved %s storage to %s (pinned via %s — reconcile will not move it back)\n",
		args[0], to, stacks.StackPinKey(args[0]),
	)
	return nil
}
