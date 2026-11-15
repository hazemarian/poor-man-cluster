package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"

	"github.com/rs/zerolog"
)

// Shared test support: a zerolog sink that keeps the lines, so a test can assert
// what the operator would have read rather than what a function returned.
// logCapture is a zerolog sink a test can read back, so the warning is
// asserted as emitted output rather than as a string in the source.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) logger() zerolog.Logger {
	return zerolog.New(&lockedWriter{c: c})
}

func (c *logCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

type logEntry struct {
	Level   string
	Message string
	Fields  map[string]json.RawMessage
}

// find returns the first entry whose message contains substr.
func (c *logCapture) find(substr string) *logEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, line := range strings.Split(strings.TrimSpace(c.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		var msg string
		_ = json.Unmarshal(raw["message"], &msg)
		if !strings.Contains(msg, substr) {
			continue
		}
		var lvl string
		_ = json.Unmarshal(raw["level"], &lvl)
		return &logEntry{Level: lvl, Message: msg, Fields: raw}
	}
	return nil
}

type lockedWriter struct{ c *logCapture }

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	return w.c.buf.Write(p)
}
