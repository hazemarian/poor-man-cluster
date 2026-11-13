// Package nodestat turns one node's own /proc into a small, fixed-size sample
// of what is eating that machine, ships it to the collector as OTLP metrics,
// and (on the leader) reads the collector's JSONL output back into one row per
// node for the console overview.
//
// Why the daemon samples and the collector does not: the collector runs as a
// single task pinned to the leader, and hostmetrics can only ever read ITS OWN
// host. Per-node numbers therefore have to come from a process that lives on
// each node, i.e. the daemon. The daemon never talks to Parseable — routing
// telemetry is the collector's job, and that rule has not changed here.
package nodestat

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TopN is how many processes the panel shows per axis. Fixed at the sample
// source so the exported series and the rendered table can never disagree
// about what "top" means.
const TopN = 5

// Proc is one process as the panel shows it.
type Proc struct {
	PID    int     `json:"pid"`
	Name   string  `json:"name"`
	RSSMB  float64 `json:"rss_mb"`
	CPUPct float64 `json:"cpu_pct"`
}

// Sample is one point-in-time reading of a node: the three numbers that decide
// whether the node is in trouble, the two process lists that say what to kill.
type Sample struct {
	Hostname string `json:"hostname"`
	Role     string `json:"role"`
	Cores    int    `json:"cores"`

	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`

	MemTotalMB int64 `json:"mem_total_mb"`
	MemUsedMB  int64 `json:"mem_used_mb"`
	MemAvailMB int64 `json:"mem_avail_mb"`

	// DockerUp is 1 when this node's engine socket answered. A node can be
	// perfectly healthy at the OS level and still be unable to start or stop
	// anything, which the panel must say out loud.
	DockerUp int `json:"docker_up"`

	TopRSS []Proc `json:"top_rss"`
	TopCPU []Proc `json:"top_cpu"`

	// TS is when the reading was taken (unix seconds).
	TS int64 `json:"ts"`
}

// Sampler reads /proc under Root. Root is "/" in production and a fixture tree
// in tests; every path is joined onto it so a test never needs privileges.
type Sampler struct {
	Root     string
	Hostname string
	Role     string

	// clkTck is USER_HZ: the unit /proc/<pid>/stat reports CPU time in. It is
	// 100 on every Linux the platform supports (x86 and arm64 alike), and the
	// daemon runs on Linux only.
	clkTck float64

	prev   map[int]procTicks
	prevAt time.Time
}

type procTicks struct {
	ticks int64
}

// NewSampler builds a sampler for one host. hostname and role are echoed into
// every sample so the panel can label a row without joining the swarm API.
func NewSampler(root, hostname, role string) *Sampler {
	if root == "" {
		root = "/"
	}
	return &Sampler{Root: root, Hostname: hostname, Role: role, clkTck: 100, prev: map[int]procTicks{}}
}

// Collect reads the node once. now is passed in so tests are deterministic and
// so a caller's clock is the only one used (CPU deltas are computed against the
// previous Collect call).
//
// A missing or unreadable /proc entry degrades the sample rather than failing
// it: a node with no load data still has memory and processes worth showing,
// and the panel's job is to say what is wrong, not to refuse to speak.
func (s *Sampler) Collect(now time.Time) (Sample, error) {
	smp := Sample{Hostname: s.Hostname, Role: s.Role, DockerUp: 1, TS: now.Unix()}

	if cores, err := s.cores(); err == nil {
		smp.Cores = cores
	}
	if l1, l5, l15, err := s.load(); err == nil {
		smp.Load1, smp.Load5, smp.Load15 = l1, l5, l15
	}
	if total, avail, err := s.meminfo(); err == nil {
		smp.MemTotalMB, smp.MemAvailMB = total, avail
		smp.MemUsedMB = total - avail
	}
	procs, err := s.processes(now)
	if err != nil {
		return smp, err
	}
	smp.TopRSS = TopBy(procs, func(p Proc) float64 { return p.RSSMB }, TopN)
	smp.TopCPU = TopBy(procs, func(p Proc) float64 { return p.CPUPct }, TopN)
	return smp, nil
}

// TopBy sorts procs by value (descending) and returns at most n of them.
//
// Ties are broken by PID ascending so the list is stable across ticks: two
// processes at 3.0% must not swap places every sample, or the panel reads as
// flicker instead of as a difference.
func TopBy(procs []Proc, value func(Proc) float64, n int) []Proc {
	out := make([]Proc, 0, len(procs))
	for _, p := range procs {
		if value(p) > 0 {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		vi, vj := value(out[i]), value(out[j])
		if vi != vj {
			return vi > vj
		}
		return out[i].PID < out[j].PID
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func (s *Sampler) path(parts ...string) string {
	return filepath.Join(append([]string{s.Root}, parts...)...)
}

func (s *Sampler) cores() (int, error) {
	b, err := os.ReadFile(s.path("proc", "cpuinfo"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "processor") {
			n++
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("no processors in /proc/cpuinfo")
	}
	return n, nil
}

func (s *Sampler) load() (l1, l5, l15 float64, err error) {
	b, err := os.ReadFile(s.path("proc", "loadavg"))
	if err != nil {
		return 0, 0, 0, err
	}
	fields := strings.Fields(string(b))
	if len(fields) < 3 {
		return 0, 0, 0, fmt.Errorf("malformed /proc/loadavg: %q", string(b))
	}
	if l1, err = strconv.ParseFloat(fields[0], 64); err != nil {
		return 0, 0, 0, err
	}
	if l5, err = strconv.ParseFloat(fields[1], 64); err != nil {
		return 0, 0, 0, err
	}
	if l15, err = strconv.ParseFloat(fields[2], 64); err != nil {
		return 0, 0, 0, err
	}
	return l1, l5, l15, nil
}

// meminfo returns total and available in MiB. Available is the number the
// panel tints on: it is what the kernel says a new workload could have without
// swapping, which is the decision an operator is about to make.
func (s *Sampler) meminfo() (totalMB, availMB int64, err error) {
	b, err := os.ReadFile(s.path("proc", "meminfo"))
	if err != nil {
		return 0, 0, err
	}
	var totalKB, availKB int64 = -1, -1
	for _, line := range strings.Split(string(b), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch key {
		case "MemTotal":
			totalKB = firstKB(rest)
		case "MemAvailable":
			availKB = firstKB(rest)
		}
	}
	if totalKB <= 0 || availKB < 0 {
		return 0, 0, fmt.Errorf("malformed /proc/meminfo: total=%d available=%d", totalKB, availKB)
	}
	return totalKB / 1024, availKB / 1024, nil
}

// firstKB parses the leading "12345 kB" of a meminfo value.
func firstKB(s string) int64 {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) == 0 {
		return -1
	}
	v, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil || v < 0 {
		return -1
	}
	return v
}

// processes lists every numeric /proc entry that yields a readable stat line,
// computing each process's CPU share against the previous Collect call.
func (s *Sampler) processes(now time.Time) ([]Proc, error) {
	entries, err := os.ReadDir(s.path("proc"))
	if err != nil {
		return nil, err
	}
	elapsed := 0.0
	if !s.prevAt.IsZero() {
		elapsed = now.Sub(s.prevAt).Seconds()
	}
	next := make(map[int]procTicks, len(entries))
	out := make([]Proc, 0, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		p, ticks, ok := s.readProcess(pid)
		if !ok {
			continue
		}
		next[pid] = procTicks{ticks: ticks}
		if prev, seen := s.prev[pid]; seen && elapsed > 0 {
			// Ticks are cumulative; a process that reset (pid reuse) shows a
			// negative delta and is reported as 0 rather than as a spike.
			if delta := ticks - prev.ticks; delta > 0 {
				p.CPUPct = float64(delta) / s.clkTck / elapsed * 100
			}
		}
		out = append(out, p)
	}
	s.prev = next
	s.prevAt = now
	return out, nil
}

// readProcess parses one /proc/<pid>/stat (utime/stime) and status (VmRSS).
func (s *Sampler) readProcess(pid int) (Proc, int64, bool) {
	b, err := os.ReadFile(s.path("proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return Proc{}, 0, false
	}
	// comm is parenthesised and may contain spaces and parentheses, so the
	// fields are counted after the LAST ')', never by splitting the whole line.
	line := string(b)
	open := strings.IndexByte(line, '(')
	closeIdx := strings.LastIndexByte(line, ')')
	if open < 0 || closeIdx < open {
		return Proc{}, 0, false
	}
	name := line[open+1 : closeIdx]
	fields := strings.Fields(line[closeIdx+1:])
	// fields[0] is state (field 3); utime is field 14, stime field 15.
	if len(fields) < 13 {
		return Proc{}, 0, false
	}
	utime, err1 := strconv.ParseInt(fields[11], 10, 64)
	stime, err2 := strconv.ParseInt(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return Proc{}, 0, false
	}
	rssKB := s.rssKB(pid)
	return Proc{PID: pid, Name: name, RSSMB: float64(rssKB) / 1024}, utime + stime, true
}

func (s *Sampler) rssKB(pid int) int64 {
	if b, err := os.ReadFile(s.path("proc", strconv.Itoa(pid), "status")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
				if v := firstKB(rest); v > 0 {
					return v
				}
			}
		}
	}
	// statm: size resident shared text lib data dt (pages). Field 2 is resident.
	if b, err := os.ReadFile(s.path("proc", strconv.Itoa(pid), "statm")); err == nil {
		if f := strings.Fields(string(b)); len(f) >= 2 {
			if pages, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				return pages * int64(os.Getpagesize()) / 1024
			}
		}
	}
	return 0
}

// Stat is the row view of this sample: what the console reads, one per node.
//
// It lives here rather than in the reader because there are now TWO producers — the
// collector's JSONL file (the leader's own pipeline) and a node reporting itself over
// mTLS — and a second copy of this mapping is how a field silently stops arriving.
func (s Sample) Stat() Stat {
	return Stat{
		Hostname:   s.Hostname,
		Role:       s.Role,
		Cores:      s.Cores,
		Load1:      s.Load1,
		Load5:      s.Load5,
		Load15:     s.Load15,
		MemTotalMB: s.MemTotalMB,
		MemUsedMB:  s.MemUsedMB,
		MemAvailMB: s.MemAvailMB,
		DockerUp:   s.DockerUp,
		TopRSS:     s.TopRSS,
		TopCPU:     s.TopCPU,
		SampleTS:   s.TS,
	}
}
