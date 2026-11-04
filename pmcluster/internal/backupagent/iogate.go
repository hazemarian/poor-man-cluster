package backupagent

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ioGate refuses to freeze a filesystem that is already saturated.
//
// fsfreeze flushes every dirty page before it can quiesce the filesystem, and it
// cannot be interrupted: the thread sits in D state until writeback finishes. On
// a node whose thin pool is a loop file (every write lands twice — guest fs →
// loop → file → ext4 → disk) that flush has been measured at seventeen minutes
// on a production box, with application tasks blocked for over ten.
//
// No timeout on our side can shorten that, because there is nothing to cancel.
// The only safe move is not to enter it: if the node is already under heavy io
// pressure or holding a large dirty backlog, skip this run and say so. A missed
// hour costs nothing; a seventeen-minute write stall costs the cluster.
type ioGate struct {
	// MaxPressure is the io.pressure "some avg60" percentage above which a
	// freeze is refused. 0 disables the check.
	MaxPressure float64
	// MaxDirtyBytes is the dirty-page backlog above which a freeze is refused.
	// 0 disables the check.
	MaxDirtyBytes int64
	// Read reports the current io pressure and dirty bytes. Injectable so the
	// decision is testable without a loaded machine.
	Read func() (pressure float64, dirtyBytes int64, err error)
}

// defaultGate returns the thresholds a production node uses.
//
// 60% pressure over a minute means the device is already the bottleneck, and
// 256 MiB of dirty pages is minutes of writeback on the loop-backed pools these
// nodes use. Both are deliberately conservative: refusing a freeze is cheap.
func defaultGate() *ioGate {
	return &ioGate{
		MaxPressure:   60,
		MaxDirtyBytes: 256 << 20,
		Read:          readHostPressure,
	}
}

// NewGate builds the freeze guard, honouring operator overrides.
//
// Both values are strings because they come from the unit's Environment: an
// empty or unparseable value falls back to the default rather than silently
// disabling a guard that exists to protect the node.
func NewGate(maxPressure, maxDirtyMB string) *ioGate {
	g := defaultGate()
	if v, err := strconv.ParseFloat(strings.TrimSpace(maxPressure), 64); err == nil && v > 0 {
		g.MaxPressure = v
	}
	if v, err := strconv.ParseInt(strings.TrimSpace(maxDirtyMB), 10, 64); err == nil && v > 0 {
		g.MaxDirtyBytes = v << 20
	}
	return g
}

// check returns an error when the node must not be frozen now.
func (g *ioGate) check() error {
	if g == nil || g.Read == nil {
		return nil
	}
	pressure, dirty, err := g.Read()
	if err != nil {
		// A reading we cannot take is not a reason to skip a backup: the freeze
		// itself is the thing that must be guarded, not the measurement.
		return nil
	}
	overPressure := g.MaxPressure > 0 && pressure >= g.MaxPressure
	overDirty := g.MaxDirtyBytes > 0 && dirty >= g.MaxDirtyBytes
	if !overPressure && !overDirty {
		return nil
	}
	return fmt.Errorf("refusing to freeze: io pressure %.0f%% (limit %.0f) with %d MiB dirty (limit %d MiB) — a freeze now would stall the node for minutes; the next run will try again",
		pressure, g.MaxPressure, dirty>>20, g.MaxDirtyBytes>>20)
}

// readHostPressure reads the two numbers that predict a slow freeze.
func readHostPressure() (float64, int64, error) {
	var pressure float64
	if f, err := os.Open("/proc/pressure/io"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "some ") {
				continue
			}
			for _, field := range strings.Fields(line) {
				if v, ok := strings.CutPrefix(field, "avg60="); ok {
					pressure, _ = strconv.ParseFloat(v, 64)
				}
			}
		}
		f.Close()
	}
	var dirty int64
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "Dirty:"); ok {
				kb, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
				dirty = kb << 10
			}
		}
	}
	return pressure, dirty, nil
}
