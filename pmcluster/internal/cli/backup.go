package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/manifest"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
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
	Short: "Restore a successful backup run into the data root",
	Args:  cobra.ExactArgs(1),
	Long: `Extracts the archives of a SUCCESSFUL backup run back under the data
root (default /var/stack/data/), overwriting existing files with the same
names. Refuses runs that did not succeed.

Restores are local-first: an archive still present on this node's archive
dir is restored from disk. When it is gone (pruned, or this node never
held it) the archive is fetched from the configured offsite S3 store
(backup_s3_* settings) if one is set; otherwise the command fails loudly
and tells you where the archive lives. --from-s3 forces the fetch.

--volume narrows the restore to one volume (e.g. db_data). On a stack-scoped
run the volume is anchored to that stack, so it can never pull another
app's same-named volume; on a whole-disk run a bare volume name matches
every app's volume, so scope it as <app>/<volume>. Without --volume the
whole run is restored. Restores always run on the node that owns the
target volume root.`,
	RunE: runBackupRestore,
}

func init() {
	backupCreateCmd.Flags().Duration("timeout", 5*time.Minute, "max time to wait for the backup to finish")
	backupListCmd.Flags().Int("limit", 20, "max rows to show (0 = all)")
	backupRestoreCmd.Flags().String("dest-root", "", "data root to restore into (<dest-root>/<stack>); default: the cluster's volume_root setting (/var/stack/data when unset)")
	backupRestoreCmd.Flags().String("volume", "", "restore only this volume (e.g. db_data); empty restores the whole run")
	backupRestoreCmd.Flags().Bool("from-s3", false, "fetch the archive from the configured offsite S3 store even when a local copy exists")

	backupCmd.AddCommand(backupCreateCmd, backupListCmd, backupBrowseCmd, backupRestoreCmd)
	rootCmd.AddCommand(backupCmd)
}

// restoreDestRootDefault resolves the default restore root from the cluster's
// volume_root setting (BUG-003), falling back to the manifest default when the
// setting is unset or the store is unavailable.
func restoreDestRootDefault(ctx context.Context) string {
	if st, _, err := openStore(); err == nil {
		defer st.Close() //nolint:errcheck // read-only best-effort cleanup
		if root := st.GetSettingDefault(ctx, cluster.SettingVolumeRoot(), ""); root != "" {
			return root
		}
	}
	return manifest.DefaultVolumeRoot
}

// backupS3FromSettings converts the persisted backup_s3_* settings into the
// S3Config the backups domain uses for its offsite restore fallback.
func backupS3FromSettings(ctx context.Context, st *store.Store) backups.S3Config {
	return backups.S3Config{
		Endpoint:  st.GetSettingDefault(ctx, cluster.SettingBackupS3Endpoint(), ""),
		Bucket:    st.GetSettingDefault(ctx, cluster.SettingBackupS3Bucket(), ""),
		AccessKey: st.GetSettingDefault(ctx, cluster.SettingBackupS3AccessKey(), ""),
		SecretKey: st.GetSettingDefault(ctx, cluster.SettingBackupS3SecretKey(), ""),
		Region:    st.GetSettingDefault(ctx, cluster.SettingBackupS3Region(), "auto"),
	}
}

// backupS3Config resolves the object-store config used for store-based backup
// transit. It prefers the in-cluster SeaweedFS store (the seaweedfs_admin
// credential, published at http://127.0.0.1:8333) over the offsite
// backup_s3_* settings, matching the daemon's read preference (serve.go). Used
// by the cross-node `backup restore` path so the mover pulls the same object
// the daemon indexes. A nil cipher (or an undecryptable credential) falls back
// to the offsite settings.
func backupS3Config(ctx context.Context, st *store.Store, cipher *credentials.Cipher) backups.S3Config {
	cfg := backupS3FromSettings(ctx, st)
	if cipher == nil {
		return cfg
	}
	if mc, err := st.GetCredential(ctx, "seaweedfs_admin"); err == nil {
		if pass, derr := cipher.Decrypt(mc.PasswordCiphertext); derr == nil {
			cfg = backups.S3Config{
				Endpoint:  "http://127.0.0.1:8333",
				Bucket:    "pmcluster-backups",
				AccessKey: mc.Username,
				SecretKey: string(pass),
			}
		}
	}
	return cfg
}

// stackFromVolume returns the stack named by a `--volume <stack>/<vol>` flag,
// or "" when the volume does not identify a stack (a bare volume name or a
// whole-disk run with no --volume).
func stackFromVolume(volume string) string {
	if i := strings.IndexByte(volume, '/'); i > 0 {
		return volume[:i]
	}
	return ""
}

// restoreDestination decides where a stack-scoped backup restore must run.
//
//   - owner is the node the stack's volume is pinned to ("" = no node-pinned
//     storage, i.e. stateless or no resolver);
//   - localHost is this node's hostname;
//   - dockerOK reports whether a Docker client is available (the store mover
//     shells out to `docker service create`);
//   - storeOK reports whether an object store is configured (the mover pulls
//     the archive object from it).
//
// Returns the destination node ("" = restore locally) or a loud error when the
// volume lives on a remote node but routing is impossible — the caller must
// never silently restore into the wrong node (BUG-032).
func restoreDestination(owner, localHost string, dockerOK, storeOK bool) (string, error) {
	if owner == "" || owner == localHost {
		return "", nil
	}
	if !dockerOK {
		// No swarm client: a single-node cluster, keep the historical local
		// path unchanged.
		return "", nil
	}
	if !storeOK {
		return "", fmt.Errorf("this stack's volume lives on %s — run the restore there", owner)
	}
	return owner, nil
}

// restoreOwnerForStack returns the node the stack's volume is pinned to, or ""
// when the stack has no node-pinned storage (stateless, role-constrained, or no
// resolver configured).
func restoreOwnerForStack(ctx context.Context, svc *stacks.Service, stack string) (string, error) {
	pins, err := svc.StoragePinsForStack(ctx, stack)
	if err != nil {
		return "", fmt.Errorf("resolve storage node for %s: %w", stack, err)
	}
	if len(pins) == 0 {
		return "", nil
	}
	return pins[0], nil
}

// restoreArchiveKey returns the object-store key for a backup run's archive:
// the basename of the row's (first) archive path. A local row stores an
// absolute host path and a store-discovered row stores the bare object key;
// both share the same basename (offen uploads under the same BACKUP_FILENAME).
func restoreArchiveKey(ctx context.Context, st *store.Store, id int64) (string, error) {
	row, err := st.GetBackup(ctx, id)
	if err != nil {
		return "", fmt.Errorf("get backup %d: %w", id, err)
	}
	paths := strings.Split(row.ArchivePaths, ",")
	if len(paths) == 0 || strings.TrimSpace(paths[0]) == "" {
		return "", fmt.Errorf("backup %d has no archive path to restore from the object store", id)
	}
	return filepath.Base(strings.TrimSpace(paths[0])), nil
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
	volume, _ := cmd.Flags().GetString("volume")
	fromS3, _ := cmd.Flags().GetBool("from-s3")

	// BUG-003 fix: the restore root must default to the cluster's volume_root
	// setting (not a hardcoded /var/stack/data), or restores land in the wrong
	// tree on clusters with a custom storage root.
	if destRoot == "" {
		destRoot = restoreDestRootDefault(cmd.Context())
	}

	// BUG-032: on a multi-node cluster a stack's volume lives on a specific
	// node (a named volume whose device is <volume_root>/<stack>/<vol>).
	// Restoring into THIS node's volume root leaves the owning node's directory
	// empty — the app comes up with an empty DB while the command reports
	// success. When --volume names a stack, resolve its owning node and route
	// the restore there (or fail loudly instead of restoring into the wrong
	// node). Single-node clusters are untouched: owner == this host (or no
	// Docker client) keeps the local path unchanged.
	if stack := stackFromVolume(volume); stack != "" && remoteClient(cmd) == nil {
		target, routeErr := restoreRouteForStack(cmd, id, stack, fromS3)
		if routeErr != nil {
			return routeErr
		}
		if target != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "✅ Restored backup %d to %s:%s.\n", id, target, destRoot)
			return nil
		}
	}

	svc, closeFn, err := backendBackups(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	restored, err := svc.Restore(cmd.Context(), id, destRoot, backups.RestoreOptions{Volume: volume, FromS3: fromS3})
	if err != nil {
		return fmt.Errorf("restore backup %d: %w", id, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✅ Restored %d file(s) from backup %d to %s:%s.\n",
		restored, id, localHostname(), destRoot)
	return nil
}

// localHostname returns this node's hostname, or "local" when os.Hostname
// fails.
func localHostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "local"
}

// restoreRouteForStack resolves where a stack-scoped backup restore must land
// and, when the stack's volume lives on another node, routes the restore there
// via the object-store mover. It returns the destination node for the success
// message: "" means the restore must be done locally (the caller does it), a
// non-empty node means it was already routed there.
//
// fromS3 mirrors the `backup restore --from-s3` flag: when true the mover pulls
// the archive from the OFFSITE backup_s3_* destination instead of the in-cluster
// SeaweedFS store (the routed restore must honour --from-s3).
func restoreRouteForStack(cmd *cobra.Command, id int64, stack string, fromS3 bool) (string, error) {
	ctx := cmd.Context()

	svc, st, closeFn, err := openDeploySvc(cmd)
	if err != nil {
		// No local deploy service (uninitialised data dir, missing encryption
		// key): there is nothing to route — fall back to the local restore.
		return "", nil
	}
	defer closeFn()

	dockerOK := false
	if dc, derr := docker.New(); derr == nil {
		svc.Docker = dc
		defer func() { _ = dc.Close() }()
		dockerOK = true
	}

	// The offsite destination is always the backup_s3_* settings. The
	// in-cluster store (preferred by default) comes from the seaweedfs_admin
	// credential when one exists and decrypts; otherwise the store path falls
	// back to the offsite settings (matching the daemon's read preference).
	svc.OffsiteS3 = backupS3FromSettings(ctx, st)
	svc.S3 = backupS3FromSettings(ctx, st)
	if cfg := loadConfig(cmd); cfg != nil {
		if cipher, cerr := credentials.Open(cfg.EncryptionKeyPath()); cerr == nil {
			svc.S3 = backupS3Config(ctx, st, cipher)
		}
	}

	owner, err := restoreOwnerForStack(ctx, svc, stack)
	if err != nil {
		return "", err
	}

	// The mover pulls from the in-cluster store by default, or the offsite
	// destination when --from-s3 is set. `storeOK` is really "source configured"
	// here, so pick the destination the mover will actually use.
	sourceCfg := svc.S3
	if fromS3 {
		sourceCfg = svc.OffsiteS3
	}
	target, err := restoreDestination(owner, localHostname(), dockerOK, sourceCfg.Configured())
	if err != nil || target == "" {
		return target, err
	}

	key, err := restoreArchiveKey(ctx, st, id)
	if err != nil {
		return "", err
	}
	if err := svc.RestoreArchiveToNode(ctx, key, stack, target, fromS3); err != nil {
		return "", fmt.Errorf("restore backup %d onto %s: %w", id, target, err)
	}
	return target, nil
}
