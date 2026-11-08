package nodestat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// tailWindow is how much of the end of the file a rebuild reads. It is enough
// for every node on a large cluster to contribute at least one sample (a batch
// is a few KiB), so a rotation never leaves a node without a row, and small
// enough that a rebuild is one read on a 1-vCPU node.
const tailWindow = 256 * 1024

// Reader tails the collector's `file` exporter output and reduces it to the
// newest sample per node.
//
// It never truncates, rotates or deletes the file: the exporter holds it open,
// and truncating a file another process is appending to produces a sparse,
// NUL-filled middle that neither side can parse. Rotation (max_megabytes /
// max_backups on the exporter) is the only cleanup, and this reader's job is to
// follow it: an inode change or a size smaller than the last offset means the
// exporter rotated, and the reader rebuilds from the tail instead of insisting
// on an offset that no longer means anything.
type Reader struct {
	mu   sync.Mutex
	path string

	inode   uint64
	offset  int64
	partial []byte
	started bool

	// malformed counts lines skipped since the process started. A malformed
	// line is collector or disk damage, never a reason to stop reading: the
	// panel must survive one torn line at a rotation boundary.
	malformed int

	// last is the newest sample per node across every read, so a node that
	// stops reporting keeps its last row (the panel then shows it ageing and
	// greys it out) instead of disappearing from the table.
	last map[string]Stat
}

// Stat is one node's reduced sample: exactly what a panel row needs.
type Stat struct {
	Hostname   string
	Role       string
	Cores      int
	Load1      float64
	Load5      float64
	Load15     float64
	MemTotalMB int64
	MemUsedMB  int64
	MemAvailMB int64
	DockerUp   int
	TopRSS     []Proc
	TopCPU     []Proc
	SampleTS   int64
}

// ReadResult reports one Read call.
type ReadResult struct {
	// Stats are the nodes whose samples appeared in the newly read bytes.
	// Empty on an unchanged file — the caller already persisted them.
	Stats []Stat
	// Malformed is how many complete lines in this read were unparseable.
	Malformed int
	// Rebuilt is true when the reader had to re-read from the tail (first
	// read, inode change, or a shrink).
	Rebuilt bool
	// Missing is true when the file does not exist yet (no collector, or a
	// fresh cluster). Not an error.
	Missing bool
}

// NewReader builds a reader over path. The path is the host path the collector
// writes: /var/stack/nodestat/nodestat.jsonl in production.
func NewReader(path string) *Reader {
	return &Reader{path: path, last: map[string]Stat{}}
}

// MalformedLines is the cumulative count of skipped lines.
func (r *Reader) MalformedLines() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.malformed
}

// Last returns the newest known sample for one node.
func (r *Reader) Last(hostname string) (Stat, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.last[hostname]
	return s, ok
}

// Read consumes everything appended since the last call.
func (r *Reader) Read() (ReadResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	f, err := os.Open(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return ReadResult{Missing: true}, nil
		}
		return ReadResult{}, fmt.Errorf("open node-stat file: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return ReadResult{}, fmt.Errorf("stat node-stat file: %w", err)
	}
	inode := inodeOf(fi)
	size := fi.Size()

	var (
		lines   [][]byte
		res     ReadResult
		newPart []byte
	)

	rebuild := !r.started || inode != r.inode || size < r.offset
	switch {
	case rebuild:
		res.Rebuilt = true
		lines, newPart, err = r.readTail(f, size)
		if err != nil {
			return res, err
		}
		r.offset = size
	case size > r.offset:
		if _, err := f.Seek(r.offset, io.SeekStart); err != nil {
			return res, fmt.Errorf("seek node-stat file: %w", err)
		}
		chunk := make([]byte, size-r.offset)
		if _, err := io.ReadFull(f, chunk); err != nil {
			return res, fmt.Errorf("read node-stat file: %w", err)
		}
		r.offset = size
		lines, newPart = splitLines(append(r.partial, chunk...))
	default:
		// Nothing appended: nothing to parse. The partial line stays partial.
		return res, nil
	}

	r.started = true
	r.inode = inode
	r.partial = newPart

	fresh := make(map[string]Stat, len(r.last)+2)
	for _, line := range lines {
		stats, ok, err := decodeLine(line)
		if err != nil {
			r.malformed++
			res.Malformed++
			continue
		}
		if !ok {
			continue
		}
		for _, st := range stats {
			if cur, seen := fresh[st.Hostname]; !seen || st.SampleTS >= cur.SampleTS {
				fresh[st.Hostname] = st
			}
		}
	}
	// One row per node per read, newest sample only. Several batches for the
	// same node inside one tick must not produce several rows, and a node whose
	// only new line is older than what is already known keeps its newer row.
	for host, st := range fresh {
		if cur, seen := r.last[host]; seen && st.SampleTS < cur.SampleTS {
			continue
		}
		r.last[host] = st
		res.Stats = append(res.Stats, st)
	}
	sort.Slice(res.Stats, func(i, j int) bool { return res.Stats[i].Hostname < res.Stats[j].Hostname })
	return res, nil
}

// readTail reads the last tailWindow bytes, drops a torn leading line (the
// window can start mid-record), and returns the complete lines plus any
// trailing partial.
func (r *Reader) readTail(f *os.File, size int64) ([][]byte, []byte, error) {
	start := size - tailWindow
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, nil, fmt.Errorf("seek node-stat file tail: %w", err)
	}
	buf := make([]byte, size-start)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, nil, fmt.Errorf("read node-stat file tail: %w", err)
	}
	lines, partial := splitLines(buf)
	if start > 0 && len(lines) > 0 {
		// The first line almost certainly began before the window. Its tail
		// would decode as a fragment (or, worse, as a plausible wrong sample),
		// so it is dropped rather than guessed at.
		lines = lines[1:]
	}
	return lines, partial, nil
}

// splitLines splits on '\n' and returns the complete lines plus the trailing
// bytes that have not been terminated yet.
func splitLines(b []byte) ([][]byte, []byte) {
	var lines [][]byte
	rest := b
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			break
		}
		line := rest[:i]
		rest = rest[i+1:]
		if len(bytes.TrimSpace(line)) > 0 {
			lines = append(lines, line)
		}
	}
	return lines, append([]byte(nil), rest...)
}

func inodeOf(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

// ---------------------------------------------------------------------------
// OTLP-JSON decoding
// ---------------------------------------------------------------------------

// The shapes below are the `file` exporter's json encoding of an OTLP
// ExportMetricsServiceRequest, one per line. Only the fields this reader needs
// are modelled; unknown fields (exemplars, histograms, sums) decode away.
type otlpDoc struct {
	ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"`
}

type otlpResourceMetrics struct {
	Resource     otlpResource      `json:"resource"`
	ScopeMetrics []otlpScopeMetric `json:"scopeMetrics"`
}

type otlpResource struct {
	Attributes []otlpKV `json:"attributes"`
}

type otlpScopeMetric struct {
	Metrics []otlpMetric `json:"metrics"`
}

type otlpMetric struct {
	Name  string    `json:"name"`
	Gauge *otlpData `json:"gauge"`
}

type otlpData struct {
	DataPoints []otlpPoint `json:"dataPoints"`
}

type otlpPoint struct {
	Attributes   []otlpKV `json:"attributes"`
	AsDouble     *float64 `json:"asDouble"`
	AsInt        *jsonNum `json:"asInt"`
	TimeUnixNano jsonNum  `json:"timeUnixNano"`
}

type otlpKV struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	StringValue *string  `json:"stringValue"`
	DoubleValue *float64 `json:"doubleValue"`
	IntValue    *jsonNum `json:"intValue"`
	BoolValue   *bool    `json:"boolValue"`
}

// jsonNum accepts both spellings OTLP-JSON allows for 64-bit integers: a bare
// number and a quoted decimal string. The Go exporter writes strings, the
// Python one writes numbers, and a fixture may carry either.
type jsonNum int64

func (n *jsonNum) UnmarshalJSON(b []byte) error {
	s := string(bytes.TrimSpace(b))
	if s == "null" || s == "" {
		*n = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return err
		}
		v = int64(f)
	}
	*n = jsonNum(v)
	return nil
}

func (n jsonNum) Int64() int64 { return int64(n) }

// decodeLine turns one JSONL line into the Stat(s) it carries, one per
// resourceMetrics entry (the exporter writes one such entry per sending
// resource, so a line from one daemon holds one node and a mixed line from a
// proxy holds several). ok is false when the line carries no node metrics at
// all (an application batch that happened to land in this file), which is not
// damage and must not be counted as malformed.
func decodeLine(line []byte) ([]Stat, bool, error) {
	var doc otlpDoc
	if err := json.Unmarshal(line, &doc); err != nil {
		return nil, false, fmt.Errorf("decode otlp line: %w", err)
	}
	var out []Stat
	for _, rm := range doc.ResourceMetrics {
		if st, ok := decodeResource(rm); ok {
			out = append(out, st)
		}
	}
	return out, len(out) > 0, nil
}

// decodeResource reduces one resourceMetrics entry to one Stat.
func decodeResource(rm otlpResourceMetrics) (Stat, bool) {
	var (
		st      Stat
		haveAny bool
		byPID   = map[int]*Proc{}
		ts      int64
	)
	resourceHost := attrString(rm.Resource.Attributes, "host.name")
	resourceRole := attrString(rm.Resource.Attributes, "node.role")

	proc := func(pid int, name string) *Proc {
		if pid <= 0 {
			return nil
		}
		if p, ok := byPID[pid]; ok {
			if name != "" {
				p.Name = name
			}
			return p
		}
		p := &Proc{PID: pid, Name: name}
		byPID[pid] = p
		return p
	}

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Gauge == nil {
				continue
			}
			for _, dp := range m.Gauge.DataPoints {
				name := attrString(dp.Attributes, "host.name")
				if name == "" {
					name = resourceHost
				}
				if name == "" {
					continue
				}
				if v := dp.TimeUnixNano.Int64(); v > ts {
					ts = v
				}
				if st.Hostname == "" {
					st.Hostname = name
					st.Role = attrString(dp.Attributes, "node.role")
					if st.Role == "" {
						st.Role = resourceRole
					}
				}
				haveAny = true
				val := dp.value()
				switch m.Name {
				case MetricLoad1:
					st.Load1 = val
				case MetricLoad5:
					st.Load5 = val
				case MetricLoad15:
					st.Load15 = val
				case MetricMemTotalMB:
					st.MemTotalMB = int64(val)
				case MetricMemUsedMB:
					st.MemUsedMB = int64(val)
				case MetricMemAvailMB:
					st.MemAvailMB = int64(val)
				case MetricCores:
					st.Cores = int(val)
				case MetricDockerUp:
					st.DockerUp = int(val)
				case MetricProcessRSS:
					if p := proc(attrInt(dp.Attributes, "process.pid"), attrString(dp.Attributes, "process.name")); p != nil {
						p.RSSMB = val
					}
				case MetricProcessCPU:
					if p := proc(attrInt(dp.Attributes, "process.pid"), attrString(dp.Attributes, "process.name")); p != nil {
						p.CPUPct = val
					}
				}
			}
		}
	}
	if !haveAny || st.Hostname == "" {
		return Stat{}, false
	}
	procs := make([]Proc, 0, len(byPID))
	for _, p := range byPID {
		procs = append(procs, *p)
	}
	sort.Slice(procs, func(i, j int) bool { return procs[i].PID < procs[j].PID })
	st.TopRSS = TopBy(procs, func(p Proc) float64 { return p.RSSMB }, TopN)
	st.TopCPU = TopBy(procs, func(p Proc) float64 { return p.CPUPct }, TopN)
	// Nanoseconds → seconds. A timestamp that is already small enough to be a
	// second value (some fixtures) is left alone rather than divided into 0.
	if ts > 1e15 {
		ts /= int64(time.Second)
	}
	st.SampleTS = ts
	return st, true
}

func (dp otlpPoint) value() float64 {
	switch {
	case dp.AsDouble != nil:
		return *dp.AsDouble
	case dp.AsInt != nil:
		return float64(*dp.AsInt)
	default:
		return 0
	}
}

func attrString(kvs []otlpKV, key string) string {
	for _, kv := range kvs {
		if kv.Key != key {
			continue
		}
		if kv.Value.StringValue != nil {
			return *kv.Value.StringValue
		}
	}
	return ""
}

func attrInt(kvs []otlpKV, key string) int {
	for _, kv := range kvs {
		if kv.Key != key {
			continue
		}
		switch {
		case kv.Value.IntValue != nil:
			return int(*kv.Value.IntValue)
		case kv.Value.DoubleValue != nil:
			return int(*kv.Value.DoubleValue)
		}
	}
	return 0
}
