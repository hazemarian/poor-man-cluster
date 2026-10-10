package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	// logTailMax caps how many lines one request may return.
	logTailMax = 2000
	// logReadMax caps how much of a file is read to satisfy a tail, so a
	// multi-gigabyte log cannot be pulled into memory by one request.
	logReadMax = 4 << 20
	// logSinceMax caps how far back `since` may reach.
	logSinceMax = 7 * 24 * time.Hour
)

// LogPage is one page of the control plane's own JSON logs. Lines are
// returned verbatim (zerolog JSON), so the console can show them as-is and a
// user can pipe them through jq elsewhere.
type LogPage struct {
	File      string   `json:"file"`
	Files     []string `json:"files"`
	Lines     []string `json:"lines"`
	Truncated bool     `json:"truncated"`
}

// LogReader tails pmcluster's daily JSON log files. The path comes from the
// data dir plus a fixed filename pattern and NEVER from the request, so a
// caller cannot aim this at an arbitrary file.
type LogReader struct {
	Dir string
}

// Files lists the daily log files that exist, oldest first.
func (l *LogReader) Files() []string {
	matches, err := filepath.Glob(filepath.Join(l.Dir, "pmcluster-*.log"))
	if err != nil {
		return nil
	}
	sort.Strings(matches)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, filepath.Base(m))
	}
	return out
}

// Tail returns the last `tail` entries, optionally only entries newer than
// `since`, optionally merged across every daily file. tail is clamped to
// logTailMax and since to logSinceMax; `truncated` reports that older entries
// existed but were not returned.
func (l *LogReader) Tail(ctx context.Context, tail int, since time.Duration, all bool) (LogPage, error) {
	files := l.Files()
	page := LogPage{Files: files, Lines: []string{}}
	if len(files) == 0 {
		return page, nil
	}
	if tail <= 0 || tail > logTailMax {
		tail = logTailMax
	}
	selected := files
	if !all {
		selected = files[len(files)-1:]
	}
	page.File = selected[len(selected)-1]

	var cutoff time.Time
	if since > 0 {
		if since > logSinceMax {
			since = logSinceMax
		}
		cutoff = time.Now().Add(-since)
	}

	collected := make([]string, 0, tail*2)
	total := 0
	for _, name := range selected {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		lines, err := tailFile(filepath.Join(l.Dir, name), int64(logReadMax))
		if err != nil {
			return page, err
		}
		for _, line := range lines {
			if line == "" {
				continue
			}
			if !cutoff.IsZero() && !logLineAfter(line, cutoff) {
				continue
			}
			total++
			collected = append(collected, line)
		}
	}
	if len(collected) > tail {
		page.Truncated = true
		collected = collected[len(collected)-tail:]
	}
	page.Lines = collected
	return page, nil
}

// tailFile reads at most maxBytes from the end of the file and returns its
// lines, dropping the leading partial line when the window did not start at a
// line boundary.
func tailFile(path string, maxBytes int64) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	truncatedHead := false
	if info.Size() > maxBytes {
		if _, err := f.Seek(info.Size()-maxBytes, 0); err != nil {
			return nil, err
		}
		truncatedHead = true
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if truncatedHead && len(lines) > 0 {
		lines = lines[1:]
	}
	return lines, nil
}

// logLineAfter reports whether a zerolog JSON line carries a timestamp at or
// after the cutoff. A line without a parsable timestamp is KEPT: dropping
// real entries because they are not shaped as expected would hide exactly the
// messages worth reading.
func logLineAfter(line string, cutoff time.Time) bool {
	if !strings.HasPrefix(strings.TrimSpace(line), "{") {
		return true
	}
	var probe struct {
		Time string `json:"time"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil || probe.Time == "" {
		return true
	}
	ts, err := time.Parse(time.RFC3339, probe.Time)
	if err != nil {
		return true
	}
	return !ts.Before(cutoff)
}

// LogsHTTP mounts the read-only control-plane log route.
type LogsHTTP struct {
	Svc *LogReader
}

// Mount registers GET /api/logs.
func (h *LogsHTTP) Mount(r chi.Router) {
	r.Get("/logs", h.tail)
}

func (h *LogsHTTP) tail(res http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	tail := 200
	if v := strings.TrimSpace(q.Get("tail")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(res, http.StatusBadRequest, "tail must be a non-negative integer")
			return
		}
		tail = n
	}
	var since time.Duration
	if v := strings.TrimSpace(q.Get("since")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			writeErr(res, http.StatusBadRequest, `since must be a duration such as "24h" or "90m"`)
			return
		}
		since = d
	}
	all := q.Get("all") == "true" || q.Get("all") == "1"
	page, err := h.Svc.Tail(req.Context(), tail, since, all)
	if err != nil {
		writeErr(res, http.StatusInternalServerError, "read logs: "+err.Error())
		return
	}
	writeJSON(res, http.StatusOK, page)
}
