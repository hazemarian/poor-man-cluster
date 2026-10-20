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
	// logAggregateReadMax caps bytes read when a request spans daily files.
	// The newest files are visited first, so an all-files request still stops
	// at a useful recent window when old files are numerous or very large.
	logAggregateReadMax = 16 << 20
	// logAllFileMax caps how many daily files an all-files request examines.
	logAllFileMax = 32
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
	// TruncationReasons identifies each independent bound that cut the read.
	// Keeping the reasons explicit prevents a caller from mistaking a
	// complete-looking page for complete history.
	TruncationReasons []string `json:"truncation_reasons,omitempty"`
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
// logTailMax and since to logSinceMax. Files are visited newest first and only
// a bounded ring of matching lines is retained; `truncated` and
// TruncationReasons report every bound that cut the read.
func (l *LogReader) Tail(ctx context.Context, tail int, since time.Duration, all bool) (LogPage, error) {
	files := l.Files()
	page := LogPage{Files: files, Lines: []string{}}
	if len(files) == 0 {
		return page, nil
	}
	reasons := make([]string, 0, 4)
	if tail <= 0 {
		tail = logTailMax
	} else if tail > logTailMax {
		tail = logTailMax
		appendReason(&reasons, "row_limit")
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
			appendReason(&reasons, "since_limit")
		}
		cutoff = time.Now().Add(-since)
	}

	// Scan newest files and newest lines first. This is a bounded ring: once it
	// has tail entries, no older entry can affect the response.
	collected := make([]string, 0, tail)
	var aggregateBytes int64
	processedFiles := 0
	rowLimit := false
	for fileIndex := len(selected) - 1; fileIndex >= 0; fileIndex-- {
		name := selected[fileIndex]
		if err := ctx.Err(); err != nil {
			return page, err
		}
		if all && processedFiles >= logAllFileMax {
			appendReason(&reasons, "file_limit")
			break
		}
		remaining := int64(logAggregateReadMax) - aggregateBytes
		if remaining <= 0 {
			appendReason(&reasons, "aggregate_byte_limit")
			break
		}
		readMax := int64(logReadMax)
		if remaining < readMax {
			readMax = remaining
		}
		read, err := tailFileBounded(filepath.Join(l.Dir, name), readMax)
		if err != nil {
			return page, err
		}
		processedFiles++
		aggregateBytes += read.BytesRead
		if read.Truncated {
			appendReason(&reasons, "file_byte_limit")
		}
		for lineIndex := len(read.Lines) - 1; lineIndex >= 0; lineIndex-- {
			if lineIndex%256 == 0 {
				if err := ctx.Err(); err != nil {
					return page, err
				}
			}
			line := read.Lines[lineIndex]
			if line == "" {
				continue
			}
			if !cutoff.IsZero() && !logLineAfter(line, cutoff) {
				continue
			}
			collected = append(collected, line)
			if len(collected) == tail {
				// There are still older lines in this file, or older files in an
				// all-files request. Either way the page is a bounded slice.
				rowLimit = lineIndex > 0 || fileIndex > 0
				break
			}
		}
		if rowLimit {
			break
		}
	}
	if all && processedFiles < len(selected) && !rowLimit && len(reasons) == 0 {
		// A file can disappear during the scan. Keep the response honest if a
		// future bound is added without a corresponding reason above.
		appendReason(&reasons, "file_limit")
	}
	if rowLimit {
		appendReason(&reasons, "row_limit")
	}
	page.Truncated = len(reasons) > 0
	page.TruncationReasons = reasons
	// The scan is newest-first for bounded work; callers have historically
	// received chronological lines, so restore that order at the boundary.
	for left, right := 0, len(collected)-1; left < right; left, right = left+1, right-1 {
		collected[left], collected[right] = collected[right], collected[left]
	}
	page.Lines = collected
	return page, nil
}

// appendReason records a truncation reason once. Several daily files can hit
// the same bound, so duplicates are dropped to keep the list readable.
func appendReason(reasons *[]string, reason string) {
	for _, r := range *reasons {
		if r == reason {
			return
		}
	}
	*reasons = append(*reasons, reason)
}

type boundedTail struct {
	Lines     []string
	BytesRead int64
	Truncated bool
}

func tailFileBounded(path string, maxBytes int64) (boundedTail, error) {
	result := boundedTail{Lines: []string{}}
	if maxBytes <= 0 {
		return result, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, nil
		}
		return result, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return result, err
	}
	result.BytesRead = info.Size()
	truncatedHead := false
	if info.Size() > maxBytes {
		if _, err := f.Seek(info.Size()-maxBytes, 0); err != nil {
			return result, err
		}
		result.BytesRead = maxBytes
		result.Truncated = true
		truncatedHead = true
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return result, err
	}
	if truncatedHead && len(lines) > 0 {
		lines = lines[1:]
	}
	result.Lines = lines
	return result, nil
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
