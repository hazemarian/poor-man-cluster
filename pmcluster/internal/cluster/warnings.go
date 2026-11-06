package cluster

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// maxRecordedWarnings bounds what a single run keeps. A run that degrades in a
// loop would otherwise write an unbounded JSON blob into cluster_settings; the
// count is what matters to an operator, and the first few are the diagnosis.
const maxRecordedWarnings = 40

// warningSink tees the update output and remembers the lines that report a
// degraded step. It is deliberately a writer rather than a new API: every
// warning in this package is already a "⚠" line written to the run's output,
// so one interception point covers all of them — including the ones added
// later, which is the whole point. The convention is documented in
// apply_record.go: if a message starts with the warning marker, it lands here.
type warningSink struct {
	dst io.Writer

	mu      sync.Mutex
	buf     bytes.Buffer // current partial line
	entries []string
	dropped int
}

// Write records complete lines and passes everything through unchanged.
func (w *warningSink) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// No newline yet: keep the partial line for the next write.
			w.buf.Reset()
			w.buf.WriteString(line)
			break
		}
		w.note(strings.TrimSpace(line))
	}
	w.mu.Unlock()
	return w.dst.Write(p)
}

// Close flushes a trailing line that never got its newline.
func (w *warningSink) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if rest := strings.TrimSpace(w.buf.String()); rest != "" {
		w.note(rest)
		w.buf.Reset()
	}
}

func (w *warningSink) note(line string) {
	if !strings.HasPrefix(line, "⚠") {
		return
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "⚠"))
	if line == "" {
		return
	}
	// Consecutive duplicates are one problem, not N: a per-node loop emits the
	// same warning once per node and the count is not the information.
	if n := len(w.entries); n > 0 && w.entries[n-1] == line {
		return
	}
	if len(w.entries) >= maxRecordedWarnings {
		w.dropped++
		return
	}
	w.entries = append(w.entries, line)
}

// List returns the recorded warnings, with a final entry naming how many were
// dropped so the record never implies it holds everything.
func (w *warningSink) List() []string {
	w.Close()
	w.mu.Lock()
	defer w.mu.Unlock()
	out := append([]string(nil), w.entries...)
	if w.dropped > 0 {
		out = append(out, "(+"+itoa(w.dropped)+" more warnings not recorded)")
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
