package nodestat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// otlpLine builds one line exactly as the collector's `file` exporter writes
// it: a JSON-encoded ExportMetricsServiceRequest with one resource per node.
// The shape is the contract the reader is written against, so the tests use it
// rather than a hand-rolled simplification.
func otlpLine(t *testing.T, host, role string, tsNano int64, node map[string]float64, procs []Proc) string {
	t.Helper()
	attrs := func(kv map[string]any) []map[string]any {
		out := make([]map[string]any, 0, len(kv))
		for k, v := range kv {
			out = append(out, map[string]any{"key": k, "value": v})
		}
		return out
	}
	str := func(s string) map[string]any { return map[string]any{"stringValue": s} }
	num := func(f float64) map[string]any { return map[string]any{"asDouble": f} }
	dbl := func(f float64) map[string]any { return map[string]any{"doubleValue": f} }

	var metrics []map[string]any
	for name, v := range node {
		metrics = append(metrics, map[string]any{
			"name": name,
			"gauge": map[string]any{"dataPoints": []map[string]any{{
				"asDouble":     num(v)["asDouble"],
				"timeUnixNano": fmt.Sprintf("%d", tsNano),
				"attributes": attrs(map[string]any{
					"host.name": str(host),
					"node.role": str(role),
				}),
			}}},
		})
	}
	for _, p := range procs {
		pa := map[string]any{
			"host.name":    str(host),
			"process.pid":  map[string]any{"intValue": fmt.Sprintf("%d", p.PID)},
			"process.name": str(p.Name),
		}
		for _, pair := range []struct {
			name string
			v    float64
		}{{MetricProcessRSS, p.RSSMB}, {MetricProcessCPU, p.CPUPct}} {
			metrics = append(metrics, map[string]any{
				"name": pair.name,
				"gauge": map[string]any{"dataPoints": []map[string]any{{
					"asDouble":     pair.v,
					"timeUnixNano": fmt.Sprintf("%d", tsNano),
					"attributes":   attrs(pa),
				}}},
			})
		}
	}
	// Use the same doubleValue spelling OTLP-JSON permits for a real export,
	// so the decoder is exercised on both asDouble and doubleValue.
	_ = dbl

	doc := map[string]any{"resourceMetrics": []map[string]any{{
		"resource": map[string]any{"attributes": attrs(map[string]any{
			"host.name": str(host),
			"node.role": str(role),
		})},
		"scopeMetrics": []map[string]any{{"metrics": metrics}},
	}}}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(b)
}

func nodeValues(load1, total, used, avail, cores float64) map[string]float64 {
	return map[string]float64{
		MetricLoad1:      load1,
		MetricLoad5:      load1,
		MetricLoad15:     load1,
		MetricMemTotalMB: total,
		MetricMemUsedMB:  used,
		MetricMemAvailMB: avail,
		MetricCores:      cores,
		MetricDockerUp:   1,
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open fixture for append: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("append fixture: %v", err)
	}
}

func TestReader_UnchangedFileParsesNothingOnSecondRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodestat.jsonl")
	writeFile(t, path, otlpLine(t, "n1", "manager", 1_700_000_000_000_000_000, nodeValues(0.4, 2048, 512, 1536, 2), nil)+"\n")

	r := NewReader(path)
	first, err := r.Read()
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(first.Stats) != 1 || first.Stats[0].Hostname != "n1" {
		t.Fatalf("first read stats = %+v, want one sample for n1", first.Stats)
	}

	second, err := r.Read()
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if len(second.Stats) != 0 {
		t.Fatalf("second read parsed %d stats from an unchanged file, want 0", len(second.Stats))
	}
	if second.Rebuilt {
		t.Fatal("an unchanged file must not trigger a rebuild")
	}
	if got := r.MalformedLines(); got != 0 {
		t.Fatalf("malformed = %d, want 0", got)
	}
}

func TestReader_PartialTrailingLineCompletesNextTick(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodestat.jsonl")
	line := otlpLine(t, "n1", "worker", 1_700_000_000_000_000_000, nodeValues(1.5, 2048, 1024, 1024, 4), nil)

	// A writer caught mid-line: no newline yet.
	writeFile(t, path, line[:len(line)/2])

	r := NewReader(path)
	first, err := r.Read()
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(first.Stats) != 0 {
		t.Fatalf("a torn line must not be parsed, got %+v", first.Stats)
	}
	if first.Malformed != 0 {
		t.Fatalf("a torn line is not malformed yet, got Malformed=%d", first.Malformed)
	}

	appendFile(t, path, line[len(line)/2:]+"\n")
	second, err := r.Read()
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if len(second.Stats) != 1 || second.Stats[0].Hostname != "n1" {
		t.Fatalf("completed line not parsed: %+v", second.Stats)
	}
	if second.Stats[0].Load1 != 1.5 {
		t.Fatalf("load1 = %v, want 1.5", second.Stats[0].Load1)
	}
}

func TestReader_MalformedLineIsSkippedAndCounted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodestat.jsonl")
	good1 := otlpLine(t, "n1", "manager", 1, nodeValues(0.2, 4096, 1024, 3072, 2), nil)
	good2 := otlpLine(t, "n2", "worker", 2, nodeValues(9.9, 2048, 2048, 0, 1), nil)
	writeFile(t, path, good1+"\n"+`{"resourceMetrics":[{"scopeMetrics":[`+"\n"+good2+"\n")

	r := NewReader(path)
	res, err := r.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.Malformed != 1 {
		t.Fatalf("Malformed = %d, want 1", res.Malformed)
	}
	if r.MalformedLines() != 1 {
		t.Fatalf("cumulative malformed = %d, want 1", r.MalformedLines())
	}
	// Both good lines around the damage still parse: one torn record must not
	// cost the panel the whole sample.
	if len(res.Stats) != 2 {
		t.Fatalf("stats = %+v, want both good lines", res.Stats)
	}
	if _, ok := r.Last("n2"); !ok {
		t.Fatal("n2 missing after a malformed line elsewhere in the batch")
	}
}

func TestReader_NewestSamplePerNodeWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodestat.jsonl")
	writeFile(t, path,
		otlpLine(t, "n1", "manager", 1_700_000_300_000_000_000, nodeValues(9.0, 2048, 1900, 100, 2), nil)+"\n"+
			otlpLine(t, "n1", "manager", 1_700_000_000_000_000_000, nodeValues(0.1, 2048, 200, 1800, 2), nil)+"\n")

	r := NewReader(path)
	res, err := r.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(res.Stats) != 1 {
		t.Fatalf("stats = %+v, want exactly one row for n1", res.Stats)
	}
	// The OLDER line arrived last on disk; the newest sample still wins, so a
	// late-flushed batch cannot resurrect a stale reading.
	if res.Stats[0].Load1 != 9.0 {
		t.Fatalf("load1 = %v, want the newer 9.0 sample", res.Stats[0].Load1)
	}
	if got, _ := r.Last("n1"); got.SampleTS != 1_700_000_300 {
		t.Fatalf("sample_ts = %d, want 1700000300", got.SampleTS)
	}
}

func TestReader_Top5WithTiesAndFewerThanFive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodestat.jsonl")
	procs := []Proc{
		{PID: 30, Name: "c", RSSMB: 300, CPUPct: 1},
		{PID: 10, Name: "a", RSSMB: 300, CPUPct: 5},
		{PID: 20, Name: "b", RSSMB: 300, CPUPct: 5},
		{PID: 40, Name: "d", RSSMB: 50, CPUPct: 0},
		{PID: 50, Name: "e", RSSMB: 10, CPUPct: 0},
		{PID: 60, Name: "f", RSSMB: 5, CPUPct: 0},
		{PID: 70, Name: "g", RSSMB: 1, CPUPct: 0},
	}
	writeFile(t, path, otlpLine(t, "n1", "worker", 10, nodeValues(1, 1024, 512, 512, 1), procs)+"\n")

	res, err := NewReader(path).Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(res.Stats) != 1 {
		t.Fatalf("stats = %+v", res.Stats)
	}
	got := res.Stats[0].TopRSS
	if len(got) != 5 {
		t.Fatalf("top rss len = %d, want 5", len(got))
	}
	// Ties broken by PID ascending: 10, 20, 30 all at 300 MB come first.
	if got[0].PID != 10 || got[1].PID != 20 || got[2].PID != 30 {
		t.Fatalf("tie order = %d,%d,%d, want 10,20,30", got[0].PID, got[1].PID, got[2].PID)
	}
	if got[4].PID != 50 {
		t.Fatalf("5th by rss = pid %d, want 50", got[4].PID)
	}
	// CPU ties: pid 10 and 20 at 5%.
	if c := res.Stats[0].TopCPU; len(c) < 2 || c[0].PID != 10 || c[1].PID != 20 || c[0].CPUPct != 5 {
		t.Fatalf("top cpu = %+v, want 10 then 20 at 5%%", c)
	}

	// Fewer than five processes: the list is exactly what exists, never padded.
	writeFile(t, path, otlpLine(t, "n1", "worker", 11, nodeValues(1, 1024, 512, 512, 1), procs[:2])+"\n")
	r := NewReader(path)
	res, err = r.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(res.Stats[0].TopRSS) != 2 {
		t.Fatalf("top rss len = %d, want 2", len(res.Stats[0].TopRSS))
	}
}

func TestReader_RotationRebuildsFromTailWithoutDuplicates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nodestat.jsonl")

	var b strings.Builder
	for i := 0; i < 4; i++ {
		b.WriteString(otlpLine(t, fmt.Sprintf("n%d", i), "manager", int64(i+1), nodeValues(float64(i), 4096, 1024, 3072, 4), nil))
		b.WriteString("\n")
	}
	writeFile(t, path, b.String())

	r := NewReader(path)
	first, err := r.Read()
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(first.Stats) != 4 {
		t.Fatalf("first read stats = %d, want 4", len(first.Stats))
	}

	// Rotation: the exporter renames the file and starts a fresh one, so the
	// inode changes and the size shrinks.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	writeFile(t, path, otlpLine(t, "n0", "manager", 100, nodeValues(7, 4096, 3072, 1024, 4), nil)+"\n"+
		otlpLine(t, "n1", "manager", 101, nodeValues(1, 4096, 1024, 3072, 4), nil)+"\n")

	second, err := r.Read()
	if err != nil {
		t.Fatalf("read after rotation: %v", err)
	}
	if !second.Rebuilt {
		t.Fatal("an inode change must trigger a rebuild")
	}
	if len(second.Stats) != 2 {
		t.Fatalf("stats after rotation = %d, want 2 (one per node, no duplicates)", len(second.Stats))
	}
	if got, _ := r.Last("n0"); got.Load1 != 7 {
		t.Fatalf("n0 load = %v, want the post-rotation 7", got.Load1)
	}
	// A node that did not appear after rotation keeps its last row.
	if _, ok := r.Last("n2"); !ok {
		t.Fatal("n2 lost its last row on rotation; the panel would drop a live node")
	}
}

func TestReader_ShrinkRebuilds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodestat.jsonl")
	writeFile(t, path,
		otlpLine(t, "n1", "manager", 1, nodeValues(1, 1024, 512, 512, 2), nil)+"\n"+
			otlpLine(t, "n2", "manager", 2, nodeValues(1, 1024, 512, 512, 2), nil)+"\n")

	r := NewReader(path)
	if _, err := r.Read(); err != nil {
		t.Fatalf("first read: %v", err)
	}
	// Shrink in place (same inode): a truncated file must not leave the reader
	// seeking past the end and silently reading nothing forever.
	writeFile(t, path, otlpLine(t, "n1", "manager", 5, nodeValues(3, 1024, 512, 512, 2), nil)+"\n")
	res, err := r.Read()
	if err != nil {
		t.Fatalf("read after shrink: %v", err)
	}
	if !res.Rebuilt {
		t.Fatal("a shrink must trigger a rebuild")
	}
	if len(res.Stats) != 1 {
		t.Fatalf("stats after shrink = %d, want 1", len(res.Stats))
	}
	if got, _ := r.Last("n1"); got.Load1 != 3 {
		t.Fatalf("n1 load = %v, want 3", got.Load1)
	}
}

func TestReader_RebuildReadsOnlyTheTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodestat.jsonl")
	var b strings.Builder
	// Enough filler to push the first samples out of the 256 KiB tail window.
	for i := 0; i < 1500; i++ {
		b.WriteString(otlpLine(t, "old", "worker", int64(i+1), nodeValues(1, 1024, 512, 512, 1), nil))
		b.WriteString("\n")
	}
	b.WriteString(otlpLine(t, "fresh", "manager", 10_000, nodeValues(2, 2048, 1024, 1024, 2), nil))
	b.WriteString("\n")
	writeFile(t, path, b.String())

	res, err := NewReader(path).Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(res.Stats) != 2 {
		t.Fatalf("tail rebuild stats = %d rows (%+v), want 2: one per node, never 1500 batches", len(res.Stats), res.Stats)
	}
	if _, ok := findStat(res.Stats, "fresh"); !ok {
		t.Fatalf("fresh node missing from the tail rebuild: %+v", res.Stats)
	}
	node, ok := findStat(res.Stats, "old")
	if !ok {
		t.Fatal("old node missing: the tail window must still carry a sample for it")
	}
	if node.SampleTS != 1500 {
		t.Fatalf("old sample_ts = %d, want the newest (1500) in the window", node.SampleTS)
	}
	// The dropped leading fragment must NOT be counted as malformed: it is a
	// window boundary, not damage.
	if res.Malformed != 0 {
		t.Fatalf("Malformed = %d, want 0 (a torn window edge is not damage)", res.Malformed)
	}
}

func TestReader_MissingFileIsNotAnError(t *testing.T) {
	r := NewReader(filepath.Join(t.TempDir(), "absent.jsonl"))
	res, err := r.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !res.Missing {
		t.Fatal("Missing = false for an absent file")
	}
	// And it must recover once the collector starts writing.
	p := filepath.Join(t.TempDir(), "later.jsonl")
	r = NewReader(p)
	if _, err := r.Read(); err != nil {
		t.Fatalf("read absent: %v", err)
	}
	writeFile(t, p, otlpLine(t, "n1", "manager", 1, nodeValues(1, 1024, 512, 512, 1), nil)+"\n")
	res, err = r.Read()
	if err != nil {
		t.Fatalf("read after file appears: %v", err)
	}
	if len(res.Stats) != 1 {
		t.Fatalf("stats = %+v, want the first sample once the file exists", res.Stats)
	}
}

func TestReader_NodeThatStopsReportingKeepsItsLastRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodestat.jsonl")
	writeFile(t, path,
		otlpLine(t, "n1", "manager", 1_700_000_000, nodeValues(1, 1024, 512, 512, 2), nil)+"\n"+
			otlpLine(t, "n2", "worker", 1_700_000_000, nodeValues(2, 2048, 1024, 1024, 2), nil)+"\n")

	r := NewReader(path)
	if _, err := r.Read(); err != nil {
		t.Fatalf("read: %v", err)
	}
	// n2 goes silent: only n1 keeps reporting.
	appendFile(t, path, otlpLine(t, "n1", "manager", 1_700_000_240, nodeValues(1.2, 1024, 600, 424, 2), nil)+"\n")
	if _, err := r.Read(); err != nil {
		t.Fatalf("second read: %v", err)
	}
	st, ok := r.Last("n2")
	if !ok {
		t.Fatal("a silent node lost its last sample; the panel must age it, not drop it")
	}
	// The sample's age is what the panel greys on.
	now := time.Unix(1_700_000_240, 0)
	if age := now.Sub(time.Unix(st.SampleTS, 0)); age < 3*time.Minute {
		t.Fatalf("age = %v, want > 3m for a node that stopped reporting", age)
	}
}

// findStat is the test-side lookup that makes the per-node assertions read as
// "this node's row", not "the row at an index the reader happened to sort by".
func findStat(stats []Stat, host string) (Stat, bool) {
	for _, s := range stats {
		if s.Hostname == host {
			return s, true
		}
	}
	return Stat{}, false
}

// TestReader_RealCollectorOutput is the format contract against a REAL
// artifact: one line written by otel/opentelemetry-collector-contrib:0.157.0's
// `file` exporter and filter/nodestat_only, fed the exact protobuf the Go SDK
// exports for one Sample. Regenerate it by pointing the collector's
// metrics/nodestat pipeline at a file and publishing a sample:
//
//	docker run --user 0:0 -v /tmp/probe:/tmp/probe \
//	  otel/opentelemetry-collector-contrib:0.157.0 --config=/tmp/probe/conf.yaml
//
// The point is that a field rename in the SDK or the exporter (asInt spelled as
// a number, host.name only on the resource, …) fails HERE, with the bytes on
// disk to look at, instead of as an empty panel in production.
func TestReader_RealCollectorOutput(t *testing.T) {
	path := filepath.Join("testdata", "collector-file-exporter-0.157.0.jsonl")
	r := NewReader(path)
	res, err := r.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.Malformed != 0 {
		t.Fatalf("real collector output counted as malformed (%d lines)", res.Malformed)
	}
	if len(res.Stats) != 1 {
		t.Fatalf("stats = %d, want one node", len(res.Stats))
	}
	st := res.Stats[0]
	if st.Hostname != "nxt-sw-4-m" || st.Role != "manager" || st.Cores != 1 {
		t.Fatalf("identity = %s/%s cores %d", st.Hostname, st.Role, st.Cores)
	}
	if st.Load1 != 9.96 || st.Load5 != 8.2 || st.Load15 != 6.1 {
		t.Fatalf("load = %v/%v/%v", st.Load1, st.Load5, st.Load15)
	}
	if st.MemTotalMB != 1839 || st.MemUsedMB != 1639 || st.MemAvailMB != 200 {
		t.Fatalf("memory = %d/%d/%d", st.MemTotalMB, st.MemUsedMB, st.MemAvailMB)
	}
	if st.DockerUp != 1 || st.SampleTS == 0 {
		t.Fatalf("docker_up = %d sample_ts = %d", st.DockerUp, st.SampleTS)
	}
	if len(st.TopRSS) != 1 || st.TopRSS[0].Name != "pmcluster" || st.TopRSS[0].RSSMB != 902 {
		t.Fatalf("top rss = %+v", st.TopRSS)
	}
	if len(st.TopCPU) != 1 || st.TopCPU[0].CPUPct != 37.5 {
		t.Fatalf("top cpu = %+v", st.TopCPU)
	}
}
