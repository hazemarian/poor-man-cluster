package storage

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/lvmlock"
)

// Grow enlarges the backing file and extends the thin pool into it.
//
// This is the answer to the only capacity question that matters on a loop-backed
// volume: the pool cannot outgrow its file, so growth is "bigger file, repvresize,
// extend the pool". Deliberately does NOT touch the data volume's size — a thin
// volume larger than its pool is a trap for the next person, and the agent's
// 80%-pool guard is measured against allocated blocks, not the volume's nominal
// size, so there is nothing to gain by inflating it.
func grow(ctx context.Context, spec Spec) error {
	spec = spec.withDefaults()
	if spec.Mode == ModeNone {
		return fmt.Errorf("grow needs a provisioned volume")
	}
	st, err := detect(ctx, spec)
	if err != nil {
		return err
	}
	if spec.Mode == ModeLoop {
		if !st.ImageExists {
			return fmt.Errorf("no backing file at %s — run `pmcluster storage provision` first", spec.ImagePath)
		}
		if spec.Size <= st.ImageBytes {
			return fmt.Errorf("backing file is already %d bytes; --size must be larger", st.ImageBytes)
		}
		if out, err := spec.exec(ctx, "truncate", "-s", strconv.FormatInt(spec.Size, 10), spec.ImagePath); err != nil {
			return fmt.Errorf("grow backing file: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		// The kernel caches the loop device's size at attach time.
		if st.LoopDevice != "" {
			if out, err := spec.exec(ctx, "losetup", "-c", st.LoopDevice); err != nil {
				return fmt.Errorf("refresh loop size: %w (%s)", err, strings.TrimSpace(string(out)))
			}
		}
		if st.VGExists {
			if out, err := spec.exec(ctx, "pvresize", st.LoopDevice); err != nil {
				return fmt.Errorf("pvresize: %w (%s)", err, strings.TrimSpace(string(out)))
			}
		}
	}
	// Extend the pool into the VG's free space MINUS the repair reserve.
	//
	// This used to be `-l +100%FREE`, which takes the last extent: the pool ends
	// up healthy and unrepairable at the same time, because lvconvert --repair
	// allocates a replacement metadata LV before it can touch the damaged one and
	// there is nowhere to put it. The reserve is two metadata copies — the
	// repaired metadata and the backup of the old one — and it costs under 1%.
	meta, err := metadataBytes(ctx, spec)
	if err != nil {
		return err
	}
	free, err := vgFreeBytes(ctx, spec)
	if err != nil {
		return err
	}
	reserve := repairReserveFactor * meta
	extend := roundDown(free-reserve, sectorAlign)
	if extend <= 0 {
		return fmt.Errorf("nothing to extend: %s has %d bytes free and a metadata repair needs %d of them kept back (grow the backing file first: --size)",
			spec.VG, free, reserve)
	}
	if out, err := spec.exec(ctx, "lvextend", "-q", "-L", fmt.Sprintf("+%dB", extend), spec.VG+"/pool"); err != nil {
		return fmt.Errorf("extend pool: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	// Verify the reserve survived: an extend that leaves the VG at zero free
	// extents is the state this guard exists to prevent, and LVM rounds.
	after, verr := vgFreeBytes(ctx, spec)
	if verr == nil && after < reserve {
		return fmt.Errorf("extended the pool but only %d bytes stay free, less than the %d a metadata repair needs", after, reserve)
	}
	// A thin pool that grew needs its metadata checked before the next snapshot.
	if out, err := spec.exec(ctx, "lvs", "--noheadings", "-o", "lv_name,data_percent", spec.VG); err != nil {
		return fmt.Errorf("verify pool: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Grow enlarges the volume, holding the LVM lock for the whole sequence so a
// concurrent `vgs` from the daemon cannot deadlock against the `lvextend`.
func Grow(ctx context.Context, spec Spec) error {
	return lvmlock.With(ctx, func() error { return grow(ctx, spec) })
}
