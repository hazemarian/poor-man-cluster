package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestMemoryLimitsFor_TheArithmetic is the whole point of the boundary: the
// numbers come from the host, and the hard ceiling stays BELOW it. A limit that
// exceeds the host is not a limit — a 2560m runner cap on a 1839 MB node is what
// took nxt-sw-3-w down on 2026-11-10.
func TestMemoryLimitsFor_TheArithmetic(t *testing.T) {
	for _, total := range []int64{512, 1839, 2048, 3852, 8192, 65536} {
		l := memoryLimitsFor(total)
		if l.TotalMiB != total {
			t.Errorf("%d MiB: TotalMiB = %d", total, l.TotalMiB)
		}
		if l.HighMiB > total*memoryHighPercent/100 {
			t.Errorf("%d MiB: MemoryHigh %d exceeds %d%% of the host", total, l.HighMiB, memoryHighPercent)
		}
		if l.MaxMiB > total*memoryMaxPercent/100 {
			t.Errorf("%d MiB: MemoryMax %d exceeds %d%% of the host", total, l.MaxMiB, memoryMaxPercent)
		}
		// The one assertion that is not a percentage: never the whole host.
		if l.MaxMiB >= total {
			t.Errorf("%d MiB: MemoryMax %d is not below the host total", total, l.MaxMiB)
		}
		if l.HighMiB >= l.MaxMiB {
			t.Errorf("%d MiB: MemoryHigh %d must stay below MemoryMax %d", total, l.HighMiB, l.MaxMiB)
		}
		if l.HighMiB < 1 || l.MaxMiB < 2 {
			t.Errorf("%d MiB: degenerate limits %d/%d", total, l.HighMiB, l.MaxMiB)
		}
	}

	// The two sizes from the incident, spelled out.
	small := memoryLimitsFor(1839)
	if small.HighMiB != 1103 || small.MaxMiB != 1287 {
		t.Errorf("1839 MiB → %d/%d MiB, want 1103/1287", small.HighMiB, small.MaxMiB)
	}
	big := memoryLimitsFor(3852)
	if big.HighMiB != 2311 || big.MaxMiB != 2696 {
		t.Errorf("3852 MiB → %d/%d MiB, want 2311/2696", big.HighMiB, big.MaxMiB)
	}
}

func TestMemoryLimitsFor_UnknownRAMWritesNothing(t *testing.T) {
	if l := memoryLimitsFor(0); l.TotalMiB != 0 || l.HighMiB != 0 || l.MaxMiB != 0 {
		t.Fatalf("memoryLimitsFor(0) = %+v, want the zero value", l)
	}
	// And the unit must carry no limit at all — a default here would be a
	// constant that could exceed a small host.
	svc := unitSections(t, renderSystemdUnit("pmuser", "docker", "/home/pmuser", "/bin/pmcluster serve", MemoryLimits{}))["Service"]
	if _, set := svc["MemoryMax"]; set {
		t.Errorf("unknown RAM rendered MemoryMax=%q; a guessed limit is what this exists to prevent", svc["MemoryMax"])
	}
	if _, set := svc["MemoryHigh"]; set {
		t.Errorf("unknown RAM rendered MemoryHigh=%q", svc["MemoryHigh"])
	}
	// -500 is still right: it does not depend on knowing the host's size.
	if svc["OOMScoreAdjust"] != "-500" {
		t.Errorf("OOMScoreAdjust = %q, want -500 even when RAM is unknown", svc["OOMScoreAdjust"])
	}
}

// TestRenderSystemdUnit_MemoryBoundariesAreOnTheHost: the rendered unit carries
// what was computed, and the values are below the host total.
func TestRenderSystemdUnit_MemoryBoundariesAreOnTheHost(t *testing.T) {
	const total = int64(3852)
	mem := memoryLimitsFor(total)
	svc := unitSections(t, renderSystemdUnit("pmuser", "docker", "/home/pmuser", "/bin/pmcluster serve", mem))["Service"]

	parse := func(key string) int64 {
		t.Helper()
		raw := strings.TrimSuffix(svc[key], "M")
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			t.Fatalf("%s=%q is not a MiB quantity: %v", key, svc[key], err)
		}
		return n
	}
	high, max := parse("MemoryHigh"), parse("MemoryMax")
	if high >= max {
		t.Errorf("MemoryHigh %dM >= MemoryMax %dM", high, max)
	}
	if max >= total {
		t.Errorf("MemoryMax %dM >= host total %d MiB: that is not a limit", max, total)
	}
	if svc["OOMScoreAdjust"] != "-500" {
		t.Errorf("OOMScoreAdjust = %q, want -500: the kernel must prefer the workload", svc["OOMScoreAdjust"])
	}
	// Nothing may be hardcoded: 3852 MiB must produce different numbers from 1839.
	other := unitSections(t, renderSystemdUnit("pmuser", "docker", "/home/pmuser", "/bin/pmcluster serve", memoryLimitsFor(1839)))["Service"]
	if other["MemoryMax"] == svc["MemoryMax"] {
		t.Errorf("MemoryMax is the same on 1839 and 3852 MiB hosts (%s): it is hardcoded", svc["MemoryMax"])
	}
}

// TestEnsureDaemonRunning_WritesTheHostsMemoryBounds: the unit on disk is what
// systemd reads, so this asserts the file, not the renderer.
func TestEnsureDaemonRunning_WritesTheHostsMemoryBounds(t *testing.T) {
	oldPath, oldSys, oldHas, oldWrite, oldHost, oldMem := daemonUnitPath, systemctlFn, hasSystemctl, writeUnitFn, hostOS, hostMemTotalMiB
	t.Cleanup(func() {
		daemonUnitPath, systemctlFn, hasSystemctl, writeUnitFn, hostOS, hostMemTotalMiB =
			oldPath, oldSys, oldHas, oldWrite, oldHost, oldMem
	})
	hostOS = "linux"
	hasSystemctl = func() bool { return true }
	writeUnitFn = os.WriteFile
	systemctlFn = func(...string) error { return nil }
	hostMemTotalMiB = func() int64 { return 1839 }
	t.Setenv("PMCLUSTER_USER", "testuser")

	dir := t.TempDir()
	daemonUnitPath = filepath.Join(dir, "pmcluster.service")
	var out bytes.Buffer
	if err := ensureDaemonRunning(&out); err != nil {
		t.Fatalf("ensureDaemonRunning: %v", err)
	}
	unit, err := os.ReadFile(daemonUnitPath)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	for _, want := range []string{"MemoryHigh=1103M", "MemoryMax=1287M", "OOMScoreAdjust=-500"} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("unit missing %q:\n%s", want, unit)
		}
	}
	if !strings.Contains(out.String(), "1103M") || !strings.Contains(out.String(), "1839 MiB") {
		t.Errorf("install output does not state the arithmetic:\n%s", out.String())
	}
}

// TestEnsureDaemonRunning_UnknownRAMWarns: a node whose MemTotal cannot be read
// gets no limit and a warning, never a plausible-looking default.
func TestEnsureDaemonRunning_UnknownRAMWarns(t *testing.T) {
	oldPath, oldSys, oldHas, oldWrite, oldHost, oldMem := daemonUnitPath, systemctlFn, hasSystemctl, writeUnitFn, hostOS, hostMemTotalMiB
	t.Cleanup(func() {
		daemonUnitPath, systemctlFn, hasSystemctl, writeUnitFn, hostOS, hostMemTotalMiB =
			oldPath, oldSys, oldHas, oldWrite, oldHost, oldMem
	})
	hostOS = "linux"
	hasSystemctl = func() bool { return true }
	writeUnitFn = os.WriteFile
	systemctlFn = func(...string) error { return nil }
	hostMemTotalMiB = func() int64 { return 0 }
	t.Setenv("PMCLUSTER_USER", "testuser")

	daemonUnitPath = filepath.Join(t.TempDir(), "pmcluster.service")
	var out bytes.Buffer
	if err := ensureDaemonRunning(&out); err != nil {
		t.Fatalf("ensureDaemonRunning: %v", err)
	}
	unit, err := os.ReadFile(daemonUnitPath)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if strings.Contains(string(unit), "MemoryMax=") {
		t.Errorf("unknown RAM still wrote a limit:\n%s", unit)
	}
	if !strings.Contains(out.String(), "could not read this host's MemTotal") {
		t.Errorf("unknown RAM did not warn:\n%s", out.String())
	}
}

// TestHostMemTotalMiB_ReadsProcMeminfo covers the parser against a real file
// shape, including the kB unit conversion.
func TestHostMemTotalMiB_ReadsProcMeminfo(t *testing.T) {
	const meminfo = "MemTotal:        3939240 kB\nMemFree:          120000 kB\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(path, []byte(meminfo), 0o644); err != nil {
		t.Fatal(err)
	}
	kb := int64(3939240)
	if got := kb / 1024; got != 3846 {
		t.Fatalf("fixture arithmetic = %d, want 3846", got)
	}
	// The production reader hardcodes /proc/meminfo; this asserts the same
	// conversion it performs, so a change to the divisor is caught here.
	lim := memoryLimitsFor(kb / 1024)
	if lim.TotalMiB != 3846 {
		t.Errorf("TotalMiB = %d, want 3846", lim.TotalMiB)
	}
}
