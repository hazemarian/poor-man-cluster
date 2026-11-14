package nodestat

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeProc builds a /proc tree under a temp dir. Every sample test writes its
// own tree: the sampler must be exercised on the exact bytes a node produces,
// not on a live machine's drifting numbers.
func fakeProc(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	return root
}

func baseFiles() map[string]string {
	return map[string]string{
		"proc/loadavg": "9.96 8.20 6.11 3/512 4242\n",
		"proc/meminfo": "" +
			"MemTotal:        1883124 kB\n" +
			"MemFree:           51200 kB\n" +
			"MemAvailable:     204800 kB\n" +
			"Buffers:           10240 kB\n",
		"proc/cpuinfo": "processor\t: 0\nmodel name\t: Xeon\n\nprocessor\t: 1\nmodel name\t: Xeon\n",
	}
}

func TestSampler_CollectReadsOwnProc(t *testing.T) {
	files := baseFiles()
	// comm with a space and parentheses: field counting must survive it.
	files["proc/101/stat"] = "101 (pmcluster serve) S 1 101 101 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 1 0 100 0 0\n"
	files["proc/101/status"] = "Name:\tpmcluster\nVmRSS:\t   932104 kB\n"
	files["proc/202/stat"] = "202 (postgres) S 1 202 202 0 -1 4194560 100 0 0 0 10 5 0 0 20 0 1 0 100 0 0\n"
	files["proc/202/status"] = "Name:\tpostgres\nVmRSS:\t    51200 kB\n"

	s := NewSampler(fakeProc(t, files), "nxt-sw-4-m", "worker")
	smp, err := s.Collect(time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if smp.Hostname != "nxt-sw-4-m" || smp.Role != "worker" {
		t.Fatalf("identity = %s/%s", smp.Hostname, smp.Role)
	}
	if smp.Cores != 2 {
		t.Fatalf("cores = %d, want 2", smp.Cores)
	}
	if smp.Load1 != 9.96 || smp.Load5 != 8.20 || smp.Load15 != 6.11 {
		t.Fatalf("load = %v/%v/%v", smp.Load1, smp.Load5, smp.Load15)
	}
	if smp.MemTotalMB != 1838 || smp.MemAvailMB != 200 || smp.MemUsedMB != 1638 {
		t.Fatalf("memory = total %d avail %d used %d, want 1838/200/1638", smp.MemTotalMB, smp.MemAvailMB, smp.MemUsedMB)
	}
	if len(smp.TopRSS) != 2 || smp.TopRSS[0].Name != "pmcluster serve" {
		t.Fatalf("top rss = %+v, want pmcluster serve first (name must survive spaces)", smp.TopRSS)
	}
	if smp.TopRSS[0].RSSMB < 910 || smp.TopRSS[0].RSSMB > 911 {
		t.Fatalf("top rss mb = %v, want ~910", smp.TopRSS[0].RSSMB)
	}
}

func TestSampler_CPUIsDeltaBetweenCollections(t *testing.T) {
	files := baseFiles()
	// 250+50 = 300 ticks at t0; 350+150 = 500 ticks 1s later.
	files["proc/101/stat"] = "101 (worker) S 1 101 101 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 1 0 100 0 0\n"
	files["proc/101/status"] = "VmRSS:\t  102400 kB\n"

	s := NewSampler(fakeProc(t, files), "n1", "manager")
	t0 := time.Unix(1_700_000_000, 0)
	first, err := s.Collect(t0)
	if err != nil {
		t.Fatalf("first collect: %v", err)
	}
	// No previous sample: CPU is unknown, reported as 0 rather than invented.
	if len(first.TopCPU) != 0 {
		t.Fatalf("first sample top cpu = %+v, want empty (no baseline)", first.TopCPU)
	}

	// 200 more ticks in 1 second at 100 Hz = 2.0 CPU-seconds = 200%.
	files["proc/101/stat"] = "101 (worker) S 1 101 101 0 -1 4194560 100 0 0 0 350 150 0 0 20 0 1 0 100 0 0\n"
	if err := os.WriteFile(filepath.Join(s.Root, "proc/101/stat"), []byte(files["proc/101/stat"]), 0o600); err != nil {
		t.Fatalf("update stat: %v", err)
	}
	second, err := s.Collect(t0.Add(time.Second))
	if err != nil {
		t.Fatalf("second collect: %v", err)
	}
	if len(second.TopCPU) != 1 {
		t.Fatalf("top cpu = %+v, want one process", second.TopCPU)
	}
	if got := second.TopCPU[0].CPUPct; got < 199 || got > 201 {
		t.Fatalf("cpu = %v%%, want ~200%% (200 ticks in 1s at 100Hz)", got)
	}
}

func TestSampler_SurvivesAPartialProc(t *testing.T) {
	// No loadavg, no meminfo, one process: the sample still carries what was
	// readable, because a panel that refuses to speak is worse than a partial
	// one.
	files := map[string]string{
		"proc/cpuinfo":   "processor\t: 0\n",
		"proc/7/stat":    "7 (solo) S 1 7 7 0 -1 4194560 100 0 0 0 1 1 0 0 20 0 1 0 100 0 0\n",
		"proc/7/status":  "VmRSS:\t    2048 kB\n",
		"proc/notapid/x": "ignored\n",
	}
	s := NewSampler(fakeProc(t, files), "n1", "manager")
	smp, err := s.Collect(time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if smp.Cores != 1 {
		t.Fatalf("cores = %d, want 1", smp.Cores)
	}
	if smp.MemTotalMB != 0 || smp.Load1 != 0 {
		t.Fatalf("unreadable sections must stay zero, got mem=%d load=%v", smp.MemTotalMB, smp.Load1)
	}
	if len(smp.TopRSS) != 1 || smp.TopRSS[0].PID != 7 {
		t.Fatalf("top rss = %+v, want the one readable process", smp.TopRSS)
	}
}

// TestSampler_CPUIdleIsADeltaBetweenSamples is the node-utilization half of "move node
// resources to Parseable". /proc/stat holds counters since boot, so utilization exists
// only as the difference between two readings: the first sample knows nothing and must
// say so, and the second must report the idle share of what happened in between —
// otherwise a node that just started would be charted as either 0% or 100% busy.
func TestSampler_CPUIdleIsADeltaBetweenSamples(t *testing.T) {
	files := baseFiles()
	// user nice system idle iowait irq softirq steal guest guest_nice
	files["proc/stat"] = "cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 50 0 50 400 0 0 0 0 0 0\n"
	root := fakeProc(t, files)
	s := NewSampler(root, "nxt-sw-1-m", "manager")
	t0 := time.Unix(1_700_000_000, 0)

	first, err := s.Collect(t0)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if first.CPUIdleKnown {
		t.Fatalf("the first sample claims to know utilization (%v) — there is no interval yet", first.CPUIdleFrac)
	}

	// 15 s later: total jiffies 1000 -> 1100, idle 800 -> 825. So 25 of the 100 jiffies
	// that passed were idle: 25%.
	if err := os.WriteFile(filepath.Join(root, "proc", "stat"),
		[]byte("cpu  175 0 100 825 0 0 0 0 0 0\n"), 0o600); err != nil {
		t.Fatalf("rewrite /proc/stat: %v", err)
	}
	second, err := s.Collect(t0.Add(15 * time.Second))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !second.CPUIdleKnown {
		t.Fatal("the second sample has an interval and must report utilization")
	}
	if second.CPUIdleFrac < 0.2499 || second.CPUIdleFrac > 0.2501 {
		t.Fatalf("idle fraction = %v, want 0.25 (25 idle of 100 total jiffies)", second.CPUIdleFrac)
	}
}

// TestSampler_CPUIdleSurvivesAStaleOrBackwardsCounter: a fixture that rewrites the file
// with smaller counters (a reboot between samples, a wrapped value) must report UNKNOWN,
// not a negative utilization that a dashboard would draw as impossible.
func TestSampler_CPUIdleSurvivesAStaleOrBackwardsCounter(t *testing.T) {
	files := baseFiles()
	files["proc/stat"] = "cpu  100 0 100 800 0 0 0 0 0 0\n"
	root := fakeProc(t, files)
	s := NewSampler(root, "nxt-sw-4-m", "manager")
	t0 := time.Unix(1_700_000_000, 0)
	if _, err := s.Collect(t0); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "stat"),
		[]byte("cpu  10 0 10 80 0 0 0 0 0 0\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	smp, err := s.Collect(t0.Add(15 * time.Second))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if smp.CPUIdleKnown {
		t.Fatalf("a counter that went backwards produced a utilization of %v, want unknown", smp.CPUIdleFrac)
	}
}
