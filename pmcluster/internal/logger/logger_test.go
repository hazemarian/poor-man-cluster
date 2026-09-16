package logger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNew_FileOnly_WritesJSON verifies the file writer produces newline-
// delimited JSON with a level + msg + timestamp.
func TestNew_FileOnly_WritesJSON(t *testing.T) {
	dir := t.TempDir()
	logger, closer, err := New(Options{LogsDir: dir, Level: "info"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer closer.Close()

	logger.Info().Str("k", "v").Msg("hello")

	files, _ := filepath.Glob(filepath.Join(dir, "pmcluster-*.log"))
	if len(files) != 1 {
		t.Fatalf("want 1 log file, got %d", len(files))
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var entry map[string]any
	if err := json.Unmarshal(data[:len(data)-1], &entry); err != nil {
		t.Fatalf("parse JSON line: %v\n%s", err, data)
	}
	if entry["message"] != "hello" {
		t.Errorf("message = %v, want hello", entry["message"])
	}
	if entry["level"] != "info" {
		t.Errorf("level = %v, want info", entry["level"])
	}
	if entry["k"] != "v" {
		t.Errorf("k = %v, want v", entry["k"])
	}
}

// TestNew_LevelFilter verifies debug entries are dropped at info level.
func TestNew_LevelFilter(t *testing.T) {
	dir := t.TempDir()
	logger, closer, err := New(Options{LogsDir: dir, Level: "info"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer closer.Close()

	logger.Debug().Msg("hidden")
	logger.Info().Msg("shown")

	files, _ := filepath.Glob(filepath.Join(dir, "pmcluster-*.log"))
	data, _ := os.ReadFile(files[0])
	if strings.Contains(string(data), "hidden") {
		t.Errorf("debug entry leaked at info level: %s", data)
	}
	if !strings.Contains(string(data), "shown") {
		t.Errorf("info entry missing: %s", data)
	}
}

// TestDailyFileWriter_Rotation verifies that crossing midnight (UTC)
// produces a new file.
func TestDailyFileWriter_Rotation(t *testing.T) {
	dir := t.TempDir()
	day1 := time.Date(2026, 5, 10, 23, 30, 0, 0, time.UTC)
	day2 := day1.Add(2 * time.Hour)

	now := day1
	w, err := newDailyFileWriter(dir, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newDailyFileWriter: %v", err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("first\n")); err != nil {
		t.Fatalf("write 1: %v", err)
	}

	now = day2
	if _, err := w.Write([]byte("second\n")); err != nil {
		t.Fatalf("write 2: %v", err)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "pmcluster-*.log"))
	if len(files) != 2 {
		t.Fatalf("want 2 log files after rotation, got %d: %v", len(files), files)
	}
}

// TestSweep deletes only files older than RetentionDays.
func TestSweep(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)

	fresh := "pmcluster-2026-05-09.log"
	atCutoff := "pmcluster-" + now.AddDate(0, 0, -RetentionDays).Format("2006-01-02") + ".log"
	stale := "pmcluster-2026-04-01.log"
	for _, name := range []string{fresh, atCutoff, stale} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "junk.txt"), []byte("y"), 0o600); err != nil {
		t.Fatalf("write junk: %v", err)
	}

	if err := Sweep(dir, now); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, fresh)); err != nil {
		t.Errorf("fresh file removed unexpectedly: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, atCutoff)); err != nil {
		t.Errorf("at-cutoff file removed unexpectedly: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, stale)); !os.IsNotExist(err) {
		t.Errorf("stale file should be gone, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "junk.txt")); err != nil {
		t.Errorf("junk file removed unexpectedly: %v", err)
	}
}

// TestSweep_MissingDir is a no-op (no error) when the dir doesn't exist.
func TestSweep_MissingDir(t *testing.T) {
	if err := Sweep(filepath.Join(t.TempDir(), "does-not-exist"), time.Now()); err != nil {
		t.Errorf("Sweep on missing dir = %v, want nil", err)
	}
}
