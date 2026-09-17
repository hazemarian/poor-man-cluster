package cli

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/backup"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/remote"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service/impl"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Trigger and inspect on-demand volume backups (offen)",
	Long: `pmcluster wraps offen/docker-volume-backup so you can trigger an
ad-hoc snapshot from the CLI or before a deploy. The actual archives
land at /var/backups/docker-volumes/ on the host that ran the backup;
this audit log records when each run started, finished, and which
files it produced.`,
}

var backupCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Trigger an on-demand backup on the local node",
	Long: `Runs 'docker exec backup' against the local offen container and waits
for it to finish. Records the run in pmcluster's backups audit table
regardless of outcome (success or failure).`,
	RunE: runBackupCreate,
}

var backupListCmd = &cobra.Command{
	Use:   "list",
	Short: "List recorded backup runs (newest first)",
	RunE:  runBackupList,
}

func init() {
	backupCreateCmd.Flags().Duration("timeout", 5*time.Minute, "max time to wait for the backup to finish")
	backupListCmd.Flags().Int("limit", 20, "max rows to show (0 = all)")

	backupCmd.AddCommand(backupCreateCmd, backupListCmd)
	rootCmd.AddCommand(backupCmd)
}

func runBackupCreate(cmd *cobra.Command, _ []string) error {
	defer initCLITelemetry()()

	timeout, _ := cmd.Flags().GetDuration("timeout")

	if rc := remoteClient(cmd); rc != nil {
		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
		defer cancel()
		id, paths, err := remote.NewBackups(rc).Trigger(ctx, "", 0)
		if err != nil {
			return fmt.Errorf("backup failed: %w", err)
		}
		printBackupResult(cmd, id, paths)
		return nil
	}

	st, _, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	svc := impl.NewBackups(st, func(ctx context.Context) ([]string, error) {
		res, err := backup.Trigger(ctx)
		if res == nil {
			return nil, err
		}
		return res.ArchivePaths, err
	})

	id, paths, err := svc.Trigger(ctx, "", 0)
	if err != nil {
		return fmt.Errorf("backup failed (recorded as id=%d): %w", id, err)
	}
	printBackupResult(cmd, id, paths)
	return nil
}

func printBackupResult(cmd *cobra.Command, id int64, paths []string) {
	fmt.Fprintf(cmd.OutOrStdout(), "✅ Backup id=%d succeeded.\n", id)
	if len(paths) > 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "Archives:")
		for _, p := range paths {
			fmt.Fprintf(cmd.OutOrStdout(), "  - %s\n", p)
		}
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "(no archive paths parsed from offen output — see /var/backups/docker-volumes/ on the host)")
	}
}

func runBackupList(cmd *cobra.Command, _ []string) error {
	limit, _ := cmd.Flags().GetInt("limit")

	if rc := remoteClient(cmd); rc != nil {
		rows, err := remote.NewBackups(rc).List(cmd.Context(), limit)
		if err != nil {
			return fmt.Errorf("list backups: %w", err)
		}
		return printBackups(cmd, rows)
	}

	st, _, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	rows, err := impl.NewBackups(st, nil).List(cmd.Context(), limit)
	if err != nil {
		return fmt.Errorf("list backups: %w", err)
	}
	return printBackups(cmd, rows)
}

func printBackups(cmd *cobra.Command, rows []*store.Backup) error {
	if len(rows) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no backups recorded yet)")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tSTACK\tREVISION\tSTARTED\tFINISHED\tNOTES")
	for _, b := range rows {
		stack := "—"
		if b.StackName.Valid {
			stack = b.StackName.String
		}
		rev := "—"
		if b.Revision.Valid {
			rev = fmt.Sprintf("%d", b.Revision.Int64)
		}
		finished := "—"
		if b.FinishedAt.Valid {
			finished = time.Unix(b.FinishedAt.Int64, 0).Format(time.RFC3339)
		}
		notes := b.ErrorMessage
		if notes == "" {
			notes = b.ArchivePaths
		}
		if len(notes) > 60 {
			notes = notes[:57] + "..."
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			b.ID, b.Status, stack, rev,
			time.Unix(b.StartedAt, 0).Format(time.RFC3339), finished, notes,
		)
	}
	return w.Flush()
}
