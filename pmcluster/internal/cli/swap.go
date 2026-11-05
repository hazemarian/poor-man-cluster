package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Swap on a small node is insurance, not tuning.
//
// Every production node in this fleet shipped with none: 4-m has 1839 MB of RAM and
// one vCPU, and when its page cache filled, kswapd had nowhere to put anything — it
// spun at 12% of the only core while dockerd lost its swarm heartbeat and Swarm
// marked the node Down. Nothing was broken; there was simply no room to make room.
//
// A file is enough. These are not database servers that need swap to be fast; they
// need swap to survive a burst without the OOM killer choosing victims.

// swapTarget sizes the swap file for a node.
//
// Half the RAM, capped at 2 GiB: below about 2 GiB of RAM there is not much to
// evict, and above 4 GiB the kernel has enough to work with on its own. Returns 0
// when the node already has swap or has RAM enough to not need it.
func swapTarget(memTotalKB, swapTotalKB int64) int64 {
	if swapTotalKB > 0 {
		return 0
	}
	const gib = int64(1) << 30
	ram := memTotalKB * 1024
	if ram >= 8*gib {
		return 0
	}
	size := ram / 2
	if size > 2*gib {
		size = 2 * gib
	}
	if size < 512*(1<<20) {
		size = 512 * (1 << 20)
	}
	return size
}

// memInfoKB reads one field of /proc/meminfo in kilobytes.
func memInfoKB(path, field string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, field+":") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, field+":"))
		if len(fields) == 0 {
			return 0
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// execCommandFn runs one command; overridable in tests.
var execCommandFn = func(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureSwap adds a swap file when the node has none and is small enough to need
// one. It is deliberately quiet and non-fatal: a node without swap must still
// install, and an operator who manages swap themselves should not be fought.
func ensureSwap(out io.Writer) error {
	if hostOS != "linux" {
		return nil
	}
	mem := memInfoKB("/proc/meminfo", "MemTotal")
	swap := memInfoKB("/proc/meminfo", "SwapTotal")
	size := swapTarget(mem, swap)
	if size == 0 {
		return nil
	}
	if _, err := os.Stat("/swapfile"); err == nil {
		fmt.Fprintln(out, "  (/swapfile exists but is not active — enable it or remove it)")
		return nil
	}
	fmt.Fprintf(out, "  → adding %d MiB of swap (this node has %d MiB of RAM and none)\n", size>>20, mem>>10)
	steps := [][]string{
		{"fallocate", "-l", strconv.FormatInt(size, 10), "/swapfile"},
		{"chmod", "600", "/swapfile"},
		{"mkswap", "/swapfile"},
		{"swapon", "/swapfile"},
	}
	for _, s := range steps {
		if err := execCommandFn(s[0], s[1:]...); err != nil {
			// fallocate is not supported on every filesystem; a sparse dd is.
			if s[0] == "fallocate" {
				if derr := execCommandFn("dd", "if=/dev/zero", "of=/swapfile", "bs=1M",
					"count="+strconv.FormatInt(size>>20, 10), "status=none"); derr == nil {
					continue
				}
			}
			fmt.Fprintf(out, "  (could not add swap: %v)\\n", err)
			return nil
		}
	}
	if err := appendOnce("/etc/fstab", "/swapfile none swap sw 0 0"); err != nil {
		fmt.Fprintf(out, "  (swap is active but not persistent: %v)\\n", err)
	}
	fmt.Fprintln(out, "  → swap active and recorded in /etc/fstab")
	return nil
}

// appendOnce appends a line to a file unless it is already there.
func appendOnce(path, line string) error {
	b, err := os.ReadFile(path)
	if err == nil && strings.Contains(string(b), line) {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n" + line + "\n")
	return err
}
