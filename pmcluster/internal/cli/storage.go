package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/storage"
)

// `pmcluster storage` owns the volume every stack's data lives on.
//
// It exists because the platform cannot back up a directory: a copy of a tree
// applications are writing to is not a backup, it is a corruption with a
// timestamp. Making that tree a mount of its own snapshot-capable filesystem is
// the prerequisite for the backup agent, and until now nothing in the platform
// created it — a fresh install produced /var/stack/data as an ordinary
// directory on the root filesystem.
func init() {
	cmd := &cobra.Command{
		Use:   "storage",
		Short: "Provision and inspect the snapshot-capable app-data volume",
		Long: `Provision and inspect the volume that holds stack data (/var/stack/data).

A plain directory on the root filesystem cannot be snapshotted, so the backup
agent refuses to run against one. Provisioning turns it into a mount of an LVM
thin volume — by default on a loop device over a sparse file, so the platform
needs nothing from the operator — and installs the units that bring it back
after a reboot.

  pmcluster storage status
  sudo pmcluster storage provision
  sudo pmcluster storage provision --mode lvm --device /dev/vdb --size 40G`,
	}
	cmd.AddCommand(storageStatusCmd(), storageProvisionCmd(), storageGrowCmd(), storageMigrateCmd(), storageRepairCmd())
	rootCmd.AddCommand(cmd)
}

func storageSpecFromFlags(cmd *cobra.Command) (storage.Spec, error) {
	var spec storage.Spec
	mode, _ := cmd.Flags().GetString("mode")
	spec.Mode = storage.Mode(mode)
	switch spec.Mode {
	case storage.ModeLoop, storage.ModeLVM, storage.ModeNone:
	case "":
		spec.Mode = storage.ModeLoop
	default:
		return spec, fmt.Errorf("unknown --mode %q (want loop, lvm or none)", mode)
	}
	spec.VolumeRoot, _ = cmd.Flags().GetString("volume-root")
	spec.ImagePath, _ = cmd.Flags().GetString("image")
	spec.VG, _ = cmd.Flags().GetString("vg")
	spec.LV, _ = cmd.Flags().GetString("lv")
	spec.Device, _ = cmd.Flags().GetString("device")
	spec.UnitDir, _ = cmd.Flags().GetString("unit-dir")
	if raw, _ := cmd.Flags().GetString("size"); raw != "" && raw != "auto" {
		n, err := parseSize(raw)
		if err != nil {
			return spec, err
		}
		spec.Size = n
	}
	return spec, nil
}

func storageStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report whether this node can take atomic snapshots",
		Long: `Report the volume's state, and with --verify prove it can snapshot.

Without --verify this reads the layout: is the volume root a mount of LVM on its
own device, is the group present, does the backing file exist. That is a
necessary condition, not the answer — a pool whose thin metadata has drifted
reports a perfect layout and fails every lvcreate. --verify takes a snapshot and
removes it, which is the operation the backup agent performs.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			spec, err := storageSpecFromFlags(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			st, err := storage.Detect(ctx, spec)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "volume root      %s\n", st.Root)
			if st.Mounted {
				fmt.Fprintf(out, "mounted from     %s%s\n", st.Source, map[bool]string{true: "  (this platform's volume)", false: "  (NOT this platform's volume)"}[st.Ours])
			} else {
				fmt.Fprintf(out, "mounted from     — not a mount point — a directory on the root filesystem\n")
			}
			fmt.Fprintf(out, "entries          %d\n", st.Entries)
			if spec.Mode == storage.ModeLoop {
				if st.ImageExists {
					fmt.Fprintf(out, "backing file     %s (%s)\n", spec.ImagePath, humanSize(st.ImageBytes))
				} else {
					fmt.Fprintf(out, "backing file     %s (absent)\n", spec.ImagePath)
				}
				if st.LoopDevice != "" {
					fmt.Fprintf(out, "loop device      %s\n", st.LoopDevice)
				}
			}
			fmt.Fprintf(out, "volume group     %s%s\n", spec.VG, map[bool]string{true: " (present)", false: " (absent)"}[st.VGExists])
			if !st.SnapshotReady() {
				fmt.Fprintf(out, "\n✗ snapshots NOT possible: the backup agent will refuse to run here.\n  fix: sudo pmcluster storage provision\n")
				return nil
			}
			// Say what was actually checked. "snapshots possible" from the layout
			// alone was printed on a node whose every hourly lvcreate was failing.
			fmt.Fprintf(out, "\n✔ snapshots possible: %s is a snapshot-capable volume (layout only — pass --verify to take one now).\n", st.Source)
			verify, _ := cmd.Flags().GetBool("verify")
			if !verify {
				return nil
			}
			if os.Geteuid() != 0 {
				return fmt.Errorf("--verify takes a snapshot, which needs root — re-run with sudo")
			}
			start := time.Now()
			if perr := storage.Probe(ctx, spec); perr != nil {
				fmt.Fprintf(out, "\n✗ snapshot probe FAILED after %s: %v\n  the layout is fine and the pool is not: see `sudo pmcluster storage repair`\n",
					time.Since(start).Round(time.Millisecond), perr)
				return nil
			}
			fmt.Fprintf(out, "\n✔ snapshot probe: took and removed one in %s — the backup agent can do the same.\n",
				time.Since(start).Round(time.Millisecond))
			return nil
		},
	}
	addStorageFlags(cmd)
	cmd.Flags().Bool("verify", false, "take a snapshot and remove it, instead of only reading the layout (needs root)")
	return cmd
}

func storageProvisionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provision",
		Short: "Create the snapshot-capable volume and the units that keep it mounted",
		Args:  cobra.NoArgs,
		Long: `Create the snapshot-capable app-data volume.

Idempotent: on an already-provisioned node it does nothing. It refuses when the
volume root holds stack data (provisioning would shadow it, and every app would
appear to have lost its files) or when it is mounted from a volume this platform
does not own.

Sizing, when --size is left at auto: half the free space, clamped to 8 GiB..128
GiB, with the data volume at 80% of its pool — the missing fifth is what lets a
backup run's snapshot exist while the copy is in flight.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			spec, err := storageSpecFromFlags(cmd)
			if err != nil {
				return err
			}
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			if !dryRun && os.Geteuid() != 0 {
				return fmt.Errorf("provisioning needs root (loop devices, LVM, mounts) — re-run with sudo, or use --dry-run to see the plan")
			}
			if dryRun {
				st, err := storage.Detect(ctx, spec)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "would provision %s\n", st.Root)
				fmt.Fprintf(out, "  mode          %s\n", spec.Mode)
				if spec.Mode == storage.ModeLoop {
					fmt.Fprintf(out, "  backing file  %s\n", spec.ImagePath)
				}
				if spec.Mode == storage.ModeLVM {
					fmt.Fprintf(out, "  device        %s\n", spec.Device)
				}
				fmt.Fprintf(out, "  volume group  %s\n  volume        %s\n", spec.VG, spec.LV)
				if spec.Size > 0 {
					fmt.Fprintf(out, "  size          %s (%s pool, %s data)\n", humanSize(spec.Size), humanSize(storage.PoolSize(spec.Size)), humanSize(storage.DataSize(storage.PoolSize(spec.Size))))
				} else {
					fmt.Fprintf(out, "  size          auto (half of free space, 8 GiB..128 GiB)\n")
				}
				if st.Provisioned() {
					fmt.Fprintf(out, "\nnothing to do: already provisioned\n")
				} else if st.Entries > 0 {
					fmt.Fprintf(out, "\nwould REFUSE: %s already holds %d entries\n", st.Root, st.Entries)
				}
				return nil
			}

			res, err := storage.Provision(ctx, spec)
			if err != nil {
				return err
			}
			if res.AlreadyProvisioned {
				fmt.Fprintf(out, "✔ already provisioned: %s\n", res.Device)
				return nil
			}
			if spec.Mode == storage.ModeNone {
				for _, w := range res.Warnings {
					fmt.Fprintf(out, "⚠ %s\n", w)
				}
				return nil
			}
			fmt.Fprintf(out, "✔ provisioned %s\n", res.Device)
			if res.ImagePath != "" {
				fmt.Fprintf(out, "  backing file  %s (%s)\n", res.ImagePath, humanSize(res.ImageBytes))
			}
			fmt.Fprintf(out, "  pool / data   %s / %s\n", humanSize(res.PoolBytes), humanSize(res.DataBytes))
			for _, u := range res.UnitsWritten {
				fmt.Fprintf(out, "  installed     %s\n", u)
			}
			for _, w := range res.Warnings {
				fmt.Fprintf(out, "⚠ %s\n", w)
			}
			fmt.Fprintf(out, "\nNext: the backup agent can now snapshot this node (pmcluster backup-agent run).\n")
			return nil
		},
	}
	addStorageFlags(cmd)
	cmd.Flags().Bool("dry-run", false, "print the plan without touching the disk")
	return cmd
}

func storageRepairCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repair",
		Short: "Repair thin-pool metadata so this node can snapshot again",
		Long: `Repair the thin pool whose metadata no longer agrees with itself.

The failure this exists for: a backup run interrupted mid-snapshot leaves a thin
device id allocated with no volume behind it, and from then on every lvcreate on
the node fails with "already contain thin device with device_id N". The volume
still mounts, the group is still present, and the backup agent keeps running and
keeps failing — which is why --verify exists and why this command ends by taking
a snapshot rather than by trusting LVM's exit status.

lvconvert --repair needs two things people are surprised by: the pool INACTIVE
(so the volume has to be unmounted, which means stopping its writers) and free
extents in the volume group for the replacement metadata. The second is checked
before anything is touched, because a pool that fills its group cannot be
repaired at all — only rebuilt.

  sudo pmcluster storage repair --unmount
  sudo pmcluster storage repair --rebuild --unmount --allow-data-loss   # last resort, data is lost`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			spec, err := storageSpecFromFlags(cmd)
			if err != nil {
				return err
			}
			if os.Geteuid() != 0 {
				return fmt.Errorf("repairing storage needs root (LVM, unmount, mount) — re-run with sudo")
			}
			var opts storage.RepairOptions
			opts.Unmount, _ = cmd.Flags().GetBool("unmount")
			opts.Rebuild, _ = cmd.Flags().GetBool("rebuild")
			opts.AllowDataLoss, _ = cmd.Flags().GetBool("allow-data-loss")
			opts.KeepBackup, _ = cmd.Flags().GetBool("keep-backup")

			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			res, err := storage.Repair(ctx, spec, opts)
			for _, m := range res.Unmounted {
				fmt.Fprintf(out, "· unmounted %s\n", m)
			}
			if err != nil {
				return err
			}
			switch res.Mode {
			case "rebuild":
				fmt.Fprintf(out, "✔ rebuilt %s/pool (%s) and %s/%s (%s, metadata %s)\n",
					spec.VG, humanSize(res.PoolBytes), spec.VG, spec.LV, humanSize(res.DataBytes), humanSize(res.MetadataBytes))
				fmt.Fprintf(out, "  the volume is empty: every stack on it starts from nothing\n")
			default:
				fmt.Fprintf(out, "✔ repaired %s/pool metadata (%s)\n", spec.VG, humanSize(res.MetadataBytes))
				if res.BackupLV != "" {
					fmt.Fprintf(out, "  old metadata kept as %s/%s, then removed\n", spec.VG, res.BackupLV)
				}
			}
			for _, line := range strings.Split(res.RepairOutput, "\n") {
				if line = strings.TrimSpace(line); line != "" {
					fmt.Fprintf(out, "  | %s\n", line)
				}
			}
			if res.Verified {
				fmt.Fprintf(out, "\n✔ verified: a snapshot was taken and removed, so the backup agent can run here again.\n")
			}
			return nil
		},
	}
	addStorageFlags(cmd)
	cmd.Flags().Bool("unmount", false, "let the repair unmount the volume (stop its writers first)")
	cmd.Flags().Bool("rebuild", false, "destroy and re-create the volume when the metadata cannot be repaired")
	cmd.Flags().Bool("allow-data-loss", false, "acknowledge that --rebuild destroys every stack's data")
	cmd.Flags().Bool("keep-backup", false, "keep LVM's copy of the unrepaired metadata for inspection")
	return cmd
}

func storageMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Adopt a node that already holds stack data (build, copy, verify, switch)",
		Long: `Move existing stack data onto a snapshot-capable volume, in place.

This is how an existing node gets snapshots without a rebuild: the new volume is
built and mounted somewhere else, the data is copied into it while nothing is
writing, the copy is verified by file count and byte total, and only then is the
mount switched. A failure at any step before the switch leaves the live tree
untouched; after it, the old tree is preserved at <volume-root>.pre-migration-<ts>.

Drain the node first so no container is writing:

  docker node update --availability drain <node>
  sudo pmcluster storage migrate
  docker node update --availability active <node>`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			spec, err := storageSpecFromFlags(cmd)
			if err != nil {
				return err
			}
			spec.Force, _ = cmd.Flags().GetBool("force")
			if os.Geteuid() != 0 {
				return fmt.Errorf("migration needs root (loop devices, LVM, mounts) — re-run with sudo")
			}
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			res, err := storage.Migrate(ctx, spec)
			if err != nil {
				return err
			}
			if res.AlreadyProvisioned {
				fmt.Fprintf(out, "✔ already provisioned: %s (nothing to migrate)\n", res.Device)
				return nil
			}
			fmt.Fprintf(out, "✔ migrated onto %s\n", res.Device)
			fmt.Fprintf(out, "  copied        %d files, %s\n", res.Copied, humanSize(res.CopyBytes))
			fmt.Fprintf(out, "  pool / data   %s / %s\n", humanSize(res.PoolBytes), humanSize(res.DataBytes))
			if res.PreviousTree != "" {
				fmt.Fprintf(out, "  previous tree %s (delete once you are satisfied)\n", res.PreviousTree)
			}
			for _, w := range res.Warnings {
				fmt.Fprintf(out, "⚠ %s\n", w)
			}
			fmt.Fprintf(out, "\nNext: docker node update --availability active <node>\n")
			return nil
		},
	}
	addStorageFlags(cmd)
	cmd.Flags().Bool("force", false, "skip the 'a container is still writing' check (only if you froze the writers yourself)")
	return cmd
}

func storageGrowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "grow --size 64G",
		Short: "Enlarge the backing file and the thin pool",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			spec, err := storageSpecFromFlags(cmd)
			if err != nil {
				return err
			}
			if spec.Size == 0 {
				return fmt.Errorf("--size is required (the new backing-file size, e.g. 64G)")
			}
			if os.Geteuid() != 0 {
				return fmt.Errorf("growing the pool needs root — re-run with sudo")
			}
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			if err := storage.Grow(ctx, spec); err != nil {
				return err
			}
			fmt.Fprintf(out, "✔ backing file and pool grown to %s\n", humanSize(spec.Size))
			return nil
		},
	}
	addStorageFlags(cmd)
	return cmd
}

// addStorageFlags is shared by status, provision and grow so the three commands
// always describe the same volume.
func addStorageFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("mode", string(storage.ModeLoop), "loop (a file on the root filesystem), lvm (a block device) or none")
	f.String("size", "auto", "backing file size (32G, 512M) or auto")
	f.String("image", storage.DefaultImagePath, "backing file path (loop mode)")
	f.String("device", "", "block device (lvm mode)")
	f.String("vg", storage.DefaultVG, "volume group name")
	f.String("lv", storage.DefaultLV, "volume name")
	f.String("volume-root", storage.DefaultVolumeRoot, "mount point")
	f.String("unit-dir", storage.DefaultUnitDir, "where systemd units are written")
}

// parseSize accepts the sizes operators actually type: 512M, 8G, 1T, or a plain
// byte count. Binary units, because the underlying tools are.
func parseSize(raw string) (int64, error) {
	s := strings.TrimSpace(strings.ToUpper(raw))
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	for _, suffix := range []struct {
		unit string
		mult int64
	}{
		{"TIB", 1 << 40}, {"TB", 1 << 40}, {"T", 1 << 40},
		{"GIB", 1 << 30}, {"GB", 1 << 30}, {"G", 1 << 30},
		{"MIB", 1 << 20}, {"MB", 1 << 20}, {"M", 1 << 20},
		{"KIB", 1 << 10}, {"KB", 1 << 10}, {"K", 1 << 10},
	} {
		if strings.HasSuffix(s, suffix.unit) {
			mult = suffix.mult
			s = strings.TrimSuffix(s, suffix.unit)
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size %q (want 32G, 512M, 1T or bytes)", raw)
	}
	return int64(n * float64(mult)), nil
}

func humanSize(n int64) string {
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

// provisionStorageForNode is the entry point the install wizard and `join` share.
// It never fails the caller: a node without a snapshot-capable volume can still
// join a cluster and serve work, it just cannot be backed up atomically, and the
// operator needs to know that rather than have a join abort.
func provisionStorageForNode(ctx context.Context, out io.Writer, spec storage.Spec) {
	st, err := storage.Detect(ctx, spec)
	if err != nil {
		fmt.Fprintf(out, "⚠ could not inspect %s: %v\n", spec.VolumeRoot, err)
		return
	}
	if st.Provisioned() {
		fmt.Fprintf(out, "✔ app-data volume already provisioned (%s)\n", st.Source)
		return
	}
	if st.Entries > 0 {
		fmt.Fprintf(out, "⚠ %s already holds %d entries: leaving it alone (snapshots stay unavailable until the stacks are moved)\n", st.Root, st.Entries)
		return
	}
	res, err := storage.Provision(ctx, spec)
	if err != nil {
		fmt.Fprintf(out, "⚠ could not provision %s: %v\n", spec.VolumeRoot, err)
		fmt.Fprintf(out, "  snapshot backups stay unavailable on this node until it is fixed\n")
		return
	}
	fmt.Fprintf(out, "✔ app-data volume provisioned: %s (pool %s, data %s)\n", res.Device, humanSize(res.PoolBytes), humanSize(res.DataBytes))
	for _, w := range res.Warnings {
		fmt.Fprintf(out, "⚠ %s\n", w)
	}
}
