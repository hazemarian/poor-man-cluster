package cli

import "testing"

// Every production node shipped without swap: 4-m has 1839 MB and one vCPU, and
// when its page cache filled, kswapd spun at 12% of the only core while dockerd
// lost its swarm heartbeat.
func TestSwapTargetSizesSmallNodes(t *testing.T) {
	const gib = int64(1 << 20) // meminfo is in kB, so 1 GiB = 1<<20 kB
	cases := []struct {
		name          string
		memKB, swapKB int64
		want          int64
	}{
		{"1839 MB node, no swap", 1839 * 1024, 0, (1839 * 1024 * 1024) / 2},
		{"3828 MB node, no swap", 3828 * 1024, 0, (3828 * 1024 * 1024) / 2},
		{"16 GB node is left alone", 16384 * 1024, 0, 0},
		{"already has swap", 1839 * 1024, 4007 * 1024, 0},
		{"tiny node floors at 512 MiB", 600 * 1024, 0, 512 * (1 << 20)},
	}
	for _, c := range cases {
		if got := swapTarget(c.memKB, c.swapKB); got != c.want {
			t.Fatalf("%s: swapTarget = %d MiB, want %d MiB", c.name, got>>20, c.want>>20)
		}
	}
}

// Reading the real /proc/meminfo must not depend on field order or spacing.
func TestMemInfoKBReadsRealFields(t *testing.T) {
	if memInfoKB("/proc/meminfo", "MemTotal") <= 0 {
		t.Skip("no /proc/meminfo on this platform")
	}
	if got := memInfoKB("/proc/meminfo", "NoSuchField"); got != 0 {
		t.Fatalf("unknown field = %d, want 0", got)
	}
}
