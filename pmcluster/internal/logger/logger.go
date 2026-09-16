// Package logger wires zerolog with two outputs: a colorized console
// writer for interactive use, and a JSON file writer at
// <data_dir>/logs/pmcluster-YYYY-MM-DD.log for audit. Files rotate per
// UTC day and are swept after RetentionDays.
//
// CLI commands print headline content (passwords, tables) directly via
// fmt.Fprint; the logger is for status, progress, and error lines.
package logger

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// RetentionDays bounds how long log files live; longer-term retention
// belongs in OpenObserve.
const RetentionDays = 14

// Options has all-optional fields.
type Options struct {
	// LogsDir holds pmcluster-YYYY-MM-DD.log files. Empty disables file logging.
	LogsDir string

	// Level: "debug" | "info" | "warn" | "error". Default "info".
	Level string

	// Console enables a colorized writer alongside the file sink.
	Console bool

	// ConsoleOut overrides the default os.Stdout for tests.
	ConsoleOut io.Writer

	// Now is a clock seam; defaults to time.Now.
	Now func() time.Time
}

// dailyFileWriter re-opens its file when the UTC date rolls over.
type dailyFileWriter struct {
	dir string
	now func() time.Time

	mu      sync.Mutex
	f       *os.File
	dateKey string
}

func newDailyFileWriter(dir string, now func() time.Time) (*dailyFileWriter, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("logger: mkdir %s: %w", dir, err)
	}
	w := &dailyFileWriter{dir: dir, now: now}
	if err := w.rotate(now()); err != nil {
		return nil, err
	}
	return w, nil
}

// rotate must be called with mu held OR before the first Write (in newDailyFileWriter).
func (w *dailyFileWriter) rotate(t time.Time) error {
	key := t.UTC().Format("2006-01-02")
	if w.f != nil && w.dateKey == key {
		return nil
	}
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	path := filepath.Join(w.dir, "pmcluster-"+key+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("logger: open %s: %w", path, err)
	}
	w.f = f
	w.dateKey = key
	return nil
}

func (w *dailyFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.rotate(w.now()); err != nil {
		return 0, err
	}
	return w.f.Write(p)
}

// Close is safe to call mid-flight; further Writes reopen lazily.
func (w *dailyFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// New returns the logger plus a closer the caller should defer (no-op
// when file logging is disabled).
func New(opts Options) (zerolog.Logger, io.Closer, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}

	level := parseLevel(opts.Level)
	zerolog.TimeFieldFormat = time.RFC3339Nano

	var writers []io.Writer
	var closer io.Closer = noopCloser{}

	if opts.Console {
		out := opts.ConsoleOut
		if out == nil {
			out = os.Stdout
		}

		console := zerolog.ConsoleWriter{
			Out:        out,
			TimeFormat: time.RFC3339,
		}
		writers = append(writers, console)
	}

	if opts.LogsDir != "" {
		fw, err := newDailyFileWriter(opts.LogsDir, opts.Now)
		if err != nil {
			return zerolog.Nop(), noopCloser{}, err
		}
		writers = append(writers, fw)
		closer = fw
	}

	writers = append(writers, newOTelWriter())

	if len(writers) == 0 {
		return zerolog.Nop(), noopCloser{}, nil
	}

	out := io.MultiWriter(writers...)
	logger := zerolog.New(out).Level(level).With().Timestamp().Logger()
	return logger, closer, nil
}

// Sweep deletes log files older than RetentionDays. Safe to call
// concurrently with active logging — Linux/macOS keep an open fd usable
// after unlink.
func Sweep(dir string, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("sweep: read %s: %w", dir, err)
	}

	cutoff := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -RetentionDays)
	var problems []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()

		if !strings.HasPrefix(name, "pmcluster-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		dateStr := strings.TrimSuffix(strings.TrimPrefix(name, "pmcluster-"), ".log")
		t, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
				problems = append(problems, name+": "+err.Error())
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("sweep: %s", strings.Join(problems, "; "))
	}
	return nil
}

func parseLevel(s string) zerolog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return zerolog.DebugLevel
	case "warn", "warning":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	default:
		return zerolog.InfoLevel
	}
}

type noopCloser struct{}

func (noopCloser) Close() error { return nil }
