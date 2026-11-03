package storage

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Grow enlarges the backing file and extends the thin pool into it.
//
// This is the answer to the only capacity question that matters on a loop-backed
// volume: the pool cannot outgrow its file, so growth is "bigger file, repvresize,
// extend the pool". Deliberately does NOT touch the data volume's size — a thin
// volume larger than its pool is a trap for the next person, and the agent's
// 80%-pool guard is measured against allocated blocks, not the volume's nominal
// size, so there is nothing to gain by inflating it.
func Grow(ctx context.Context, spec Spec) error {
	spec = spec.withDefaults()
	if spec.Mode == ModeNone {
		return fmt.Errorf("grow needs a provisioned volume")
	}
	st, err := Detect(ctx, spec)
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
	// Extend the pool into whatever free space the VG now has.
	if out, err := spec.exec(ctx, "lvextend", "-q", "-l", "+100%FREE", spec.VG+"/pool"); err != nil {
		return fmt.Errorf("extend pool: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	// A thin pool that grew needs its metadata checked before the next snapshot.
	if out, err := spec.exec(ctx, "lvs", "--noheadings", "-o", "lv_name,data_percent", spec.VG); err != nil {
		return fmt.Errorf("verify pool: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
