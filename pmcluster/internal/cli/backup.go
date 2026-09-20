package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
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

var backupBrowseCmd = &cobra.Command{
	Use:   "browse <id>",
	Short: "List the archive contents of a backup run",
	Args:  cobra.ExactArgs(1),
	Long: `Lists every file inside a backup run's archive(s). For plain-directory
archives this is a recursive walk; for tar / tar.gz archives the tar
headers are read without extracting anything. A 'missing' size marks an
archive path that no longer exists on disk.`,
	RunE: runBackupBrowse,
}

var backupRestoreCmd = &cobra.Command{
	Use:   "restore <id>",
	Short: "Restore a successful stack-scoped backup run into the data root",
	Args:  cobra.ExactArgs(1),
	Long: `Extracts every archive of a SUCCESSFUL, stack-scoped backup run back
under the data root (default /var/stack/data/<stack>/), overwriting
existing files with the same names. Refuses runs that did not succeed
and runs not tied to a stack.`,
	RunE: runBackupRestore,
}

func init() {
	backupCreateCmd.Flags().Duration("timeout", 5*time.Minute, "max time to wait for the backup to finish")
	backupListCmd.Flags().Int("limit", 20, "max rows to show (0 = all)")
	backupRestoreCmd.Flags().String("dest-root", "/var/stack/data", "data root to restore into (<dest-root>/<stack>)")

	backupCmd.AddCommand(backupCreateCmd, backupListCmd, backupBrowseCmd, backupRestoreCmd)
	rootCmd.AddCommand(backupCmd)
}

func runBackupCreate(cmd *cobra.Command, _ []string) error {
	defer initCLITelemetry()()

	timeout, _ := cmd.Flags().GetDuration("timeout")

	svc, closeFn, err := backendBackups(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

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

	svc, closeFn, err := backendBackups(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	rows, err := svc.List(cmd.Context(), limit)
	if err != nil {
		return fmt.Errorf("list backups: %w", err)
	}
	return printBackups(cmd, rows)
}

func printBackups(cmd *cobra.Command, rows []backups.Run) error {
	if len(rows) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no backups recorded yet)")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tSTACK\tREVISION\tSTARTED\tFINISHED\tNOTES")
	for _, b := range rows {
		stack := "—"
		if b.StackName != "" {
			stack = b.StackName
		}
		rev := "—"
		if b.Revision != 0 {
			rev = fmt.Sprintf("%d", b.Revision)
		}
		finished := "—"
		if b.FinishedAt != 0 {
			finished = time.Unix(b.FinishedAt, 0).Format(time.RFC3339)
		}
		notes := b.ErrorMessage
		if notes == "" {
			notes = strings.Join(b.ArchivePaths, ",")
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

func runBackupBrowse(cmd *cobra.Command, args []string) error {
	defer initCLITelemetry()()

	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("id: must be an integer (got %q)", args[0])
	}

	svc, closeFn, err := backendBackups(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	run, files, err := svc.Browse(cmd.Context(), id)
	if err != nil {
		return fmt.Errorf("browse backup %d: %w", id, err)
	}

	status := run.Status
	stack := "—"
	if run.StackName != "" {
		stack = run.StackName
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Backup %d · status %s · stack %s\n",
		run.ID, status, stack)
	if len(files) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no archive contents found)")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TYPE\tSIZE\tPATH")
	for _, f := range files {
		kind := "file"
		if f.IsDir {
			kind = "dir"
		}
		size := fmt.Sprintf("%d", f.Size)
		if f.Size < 0 {
			size = "missing"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", kind, size, f.Path)
	}
	return w.Flush()
}

func runBackupRestore(cmd *cobra.Command, args []string) error {
	defer initCLITelemetry()()

	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("id: must be an integer (got %q)", args[0])
	}
	destRoot, _ := cmd.Flags().GetString("dest-root")

	svc, closeFn, err := backendBackups(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	restored, err := svc.Restore(cmd.Context(), id, destRoot)
	if err != nil {
		return fmt.Errorf("restore backup %d: %w", id, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✅ Restored %d file(s) from backup %d under %s.\n",
		restored, id, destRoot)
	return nil
}
