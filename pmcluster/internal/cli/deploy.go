package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
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
	deployCmd.Flags().String("version", "", "override the manifest's version (image tag)")

	stackCmd.AddCommand(stackListCmd, stackShowCmd)

	rootCmd.AddCommand(deployCmd, stackCmd, rollbackCmd)
}

// openDeploySvc shares the boilerplate across the deploy/stack commands.
// Caller MUST defer the returned closer.
func openDeploySvc(cmd *cobra.Command) (*stacks.Service, *store.Store, func(), error) {
	st, _, err := openStore()
	if err != nil {
		return nil, nil, nil, err
	}
	deployer := cluster.NewDockerCLIDeployer(cmd.OutOrStdout())
	svc := &stacks.Service{Store: st, Deployer: deployer, Backup: backups.LocalTrigger{Store: st}, Resolver: &stacks.StoreConfigResolver{Store: st}}
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
	version, _ := cmd.Flags().GetString("version")
	payload := stacks.Payload{
		AppName:  appOverride,
		RepoURL:  repo,
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
	fmt.Fprintln(w, "NAME\tREVISION\tUPDATED\tREPO")
	for _, s := range list {
		repo := "—"
		if s.RepoURL != "" {
			repo = s.RepoURL
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n",
			s.Name, s.CurrentRevision,
			time.Unix(s.UpdatedAt, 0).Format(time.RFC3339), repo,
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
