package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/deploy"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/remote"
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
func openDeploySvc(cmd *cobra.Command) (*deploy.Service, *store.Store, func(), error) {
	st, _, err := openStore()
	if err != nil {
		return nil, nil, nil, err
	}
	deployer := cluster.NewDockerCLIDeployer(cmd.OutOrStdout())
	svc := &deploy.Service{Store: st, Deployer: deployer, Backup: backups.LocalTrigger{Store: st}, Resolver: &deploy.StoreConfigResolver{Store: st}}
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
	payload := deploy.Payload{
		AppName:  appOverride,
		RepoURL:  repo,
		Version:  version,
		Manifest: string(manifestBytes),
	}

	var res *deploy.DeployResult
	if rc := remoteClient(cmd); rc != nil {
		res, err = remote.NewDeploy(rc).Deploy(cmd.Context(), payload)
	} else {
		svc, _, closeFn, openErr := openDeploySvc(cmd)
		if openErr != nil {
			return openErr
		}
		defer closeFn()
		res, err = svc.Deploy(cmd.Context(), payload)
	}
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
	var stacks []*store.Stack
	var err error
	if rc := remoteClient(cmd); rc != nil {
		stacks, err = remote.NewStacks(rc).List(cmd.Context())
	} else {
		_, st, closeFn, openErr := openDeploySvc(cmd)
		if openErr != nil {
			return openErr
		}
		defer closeFn()
		stacks, err = st.ListStacks(cmd.Context())
	}
	if err != nil {
		return fmt.Errorf("list stacks: %w", err)
	}
	if len(stacks) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no stacks yet — use `pmcluster deploy <manifest.yaml>`)")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tREVISION\tUPDATED\tREPO")
	for _, s := range stacks {
		repo := "—"
		if s.RepoURL.Valid && s.RepoURL.String != "" {
			repo = s.RepoURL.String
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

	var s *store.Stack
	var revs []*store.StackRevision
	var lastBackup string
	if rc := remoteClient(cmd); rc != nil {
		var err error
		s, err = remote.NewStacks(rc).Get(cmd.Context(), name)
		if err != nil {
			if errors.Is(err, store.ErrStackNotFound) {
				return fmt.Errorf("stack %q not found", name)
			}
			return err
		}
		revs, err = remote.NewStacks(rc).Revisions(cmd.Context(), name, 20)
		if err != nil {
			return fmt.Errorf("list revisions: %w", err)
		}
		backups, err := remote.NewBackups(rc).ListForStack(cmd.Context(), name)
		if err == nil && len(backups) > 0 {
			lastBackup = formatLastBackupRow(backups[0])
		}
	} else {
		_, st, closeFn, openErr := openDeploySvc(cmd)
		if openErr != nil {
			return openErr
		}
		defer closeFn()
		var err error
		s, err = st.GetStack(cmd.Context(), name)
		if err != nil {
			if errors.Is(err, store.ErrStackNotFound) {
				return fmt.Errorf("stack %q not found", name)
			}
			return err
		}
		revs, err = st.ListRevisions(cmd.Context(), name, 20)
		if err != nil {
			return fmt.Errorf("list revisions: %w", err)
		}
		lastBackup = formatLastBackup(cmd.Context(), st, name)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Stack:            %s\n", s.Name)
	fmt.Fprintf(out, "Current revision: %d (%s)\n", s.CurrentRevision, time.Unix(s.CurrentRevision, 0).Format(time.RFC3339))
	if s.RepoURL.Valid && s.RepoURL.String != "" {
		fmt.Fprintf(out, "Repo:             %s\n", s.RepoURL.String)
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

// formatLastBackup returns a one-line summary of the most recent backup
// for the named stack, or a "(none recorded)" placeholder. Errors are
// folded into the display string — stack show should never fail because
// the backup lookup hiccupped.
func formatLastBackup(ctx context.Context, st *store.Store, name string) string {
	backups, err := st.ListBackupsForStack(ctx, name)
	if err != nil {
		return "(lookup failed: " + err.Error() + ")"
	}
	if len(backups) == 0 {
		return "(none recorded)"
	}
	b := backups[0]
	ts := time.Unix(b.StartedAt, 0).Format(time.RFC3339)
	revPart := ""
	if b.Revision.Valid {
		revPart = fmt.Sprintf(" (rev %d)", b.Revision.Int64)
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

	var res *deploy.DeployResult
	if rc := remoteClient(cmd); rc != nil {
		res, err = remote.NewDeploy(rc).Rollback(cmd.Context(), name, rev)
	} else {
		svc, _, closeFn, openErr := openDeploySvc(cmd)
		if openErr != nil {
			return openErr
		}
		defer closeFn()
		res, err = svc.Rollback(cmd.Context(), name, rev)
	}
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
