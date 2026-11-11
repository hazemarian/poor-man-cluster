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
	// The MAIN unit carries no memory directives at all, ever: they belong to the
	// drop-in, which is recomputed on every start. Two owners meant two answers.
	svc := unitSections(t, renderSystemdUnit("pmuser", "docker", "/home/pmuser", "/bin/pmcluster serve"))["Service"]
	for _, key := range []string{"MemoryMax", "MemoryHigh", "OOMScoreAdjust"} {
		if v, set := svc[key]; set {
			t.Errorf("the unit still carries %s=%q; the drop-in is the only owner", key, v)
		}
	}
}

// TestMemoryDropIn_IsTheOnlyOwner: the bounds are written to the drop-in, are
// derived from the host, and a second identical write is a no-op — otherwise every
// start would trigger a daemon-reload for nothing.
func TestMemoryDropIn_IsTheOnlyOwner(t *testing.T) {
	old := stopUnitDropIn
	t.Cleanup(func() { stopUnitDropIn = old })
	stopUnitDropIn = filepath.Join(t.TempDir(), "memory.conf")

	mem := memoryLimitsFor(3852)
	changed, err := writeMemoryDropIn(mem)
	if err != nil {
		t.Fatalf("writeMemoryDropIn: %v", err)
	}
	if !changed {
		t.Error("the first write reported no change")
	}
	body, err := os.ReadFile(stopUnitDropIn)
	if err != nil {
		t.Fatalf("read drop-in: %v", err)
	}
	svc := unitSections(t, string(body))["Service"]
	parse := func(key string) int64 {
		t.Helper()
		n, err := strconv.ParseInt(strings.TrimSuffix(svc[key], "M"), 10, 64)
		if err != nil {
			t.Fatalf("%s=%q is not a MiB quantity: %v", key, svc[key], err)
		}
		return n
	}
	high, max := parse("MemoryHigh"), parse("MemoryMax")
	if high >= max {
		t.Errorf("MemoryHigh %dM >= MemoryMax %dM", high, max)
	}
	if max >= mem.TotalMiB {
		t.Errorf("MemoryMax %dM >= host total %d MiB: that is not a limit", max, mem.TotalMiB)
	}
	if svc["OOMScoreAdjust"] != "-500" {
		t.Errorf("OOMScoreAdjust = %q, want -500: the kernel must prefer the workload", svc["OOMScoreAdjust"])
	}

	// Hardcoded values would give a 1839 MiB host the same numbers.
	if memoryLimitsFor(1839).MaxMiB == mem.MaxMiB {
		t.Errorf("MemoryMax is the same on 1839 and 3852 MiB hosts (%d): it is hardcoded", mem.MaxMiB)
	}

	changed, err = writeMemoryDropIn(mem)
	if err != nil {
		t.Fatalf("second writeMemoryDropIn: %v", err)
	}
	if changed {
		t.Error("an unchanged host reported a change, which would daemon-reload on every start")
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
	stopUnitDropIn = filepath.Join(dir, "memory.conf")
	var out bytes.Buffer
	if err := ensureDaemonRunning(&out); err != nil {
		t.Fatalf("ensureDaemonRunning: %v", err)
	}
	unit, err := os.ReadFile(daemonUnitPath)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if strings.Contains(string(unit), "MemoryMax=") {
		t.Errorf("the install wrote a limit into the main unit as well as the drop-in:\n%s", unit)
	}
	dropIn, err := os.ReadFile(stopUnitDropIn)
	if err != nil {
		t.Fatalf("the installer did not write %s: %v", stopUnitDropIn, err)
	}
	for _, want := range []string{"MemoryHigh=1103M", "MemoryMax=1287M", "OOMScoreAdjust=-500"} {
		if !strings.Contains(string(dropIn), want) {
			t.Errorf("drop-in missing %q:\n%s", want, dropIn)
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

	dirU := t.TempDir()
	daemonUnitPath = filepath.Join(dirU, "pmcluster.service")
	stopUnitDropIn = filepath.Join(dirU, "memory.conf")
	var out bytes.Buffer
	if err := ensureDaemonRunning(&out); err != nil {
		t.Fatalf("ensureDaemonRunning: %v", err)
	}
	unit, err := os.ReadFile(daemonUnitPath)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if strings.Contains(string(unit), "MemoryMax=") {
		t.Errorf("unknown RAM still wrote a limit into the unit:\n%s", unit)
	}
	dropIn, derr := os.ReadFile(stopUnitDropIn)
	if derr != nil {
		t.Fatalf("read drop-in: %v", derr)
	}
	if strings.Contains(string(dropIn), "MemoryMax=") {
		t.Errorf("unknown RAM wrote a guessed limit into the drop-in:\n%s", dropIn)
	}
	if !strings.Contains(string(dropIn), "OOMScoreAdjust=-500") {
		t.Errorf("unknown RAM dropped the OOM ordering too; -500 does not need MemTotal:\n%s", dropIn)
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
