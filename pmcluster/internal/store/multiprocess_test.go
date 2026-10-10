package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestMultiProcessNoWALSplitBrain is a regression test for the multi-process
// WAL corruption bug: when a second process (the CLI, python3, ...) opened the
// same database in WAL mode and closed its connection, SQLite deleted the
// -wal/-shm sidecars while the long-running daemon still held them open. The
// daemon then wrote to the orphaned inode, producing a split-brain database
// (stale reads, torn indexes, invisible rows — the CLI-created users 401 bug).
//
// With journal_mode=DELETE the store commits directly to the main file, there
// are no long-lived sidecars to orphan, and a writer's close can never detach
// the main DB from another open handle.
func TestMultiProcessNoWALSplitBrain(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.db")

	// First "process": the long-running daemon.
	daemon, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open daemon store: %v", err)
	}
	defer daemon.Close()
	ctx := context.Background()
	if err := daemon.SetSetting(ctx, "probe", "daemon"); err != nil {
		t.Fatalf("daemon write: %v", err)
	}

	// No WAL or SHM sidecars may ever exist in DELETE mode.
	assertNoSidecars(t, dir)

	// Second "process": a CLI-style writer that opens, writes, and closes.
	cli, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open cli store: %v", err)
	}
	if err := cli.SetSetting(ctx, "cli_probe", "from-cli"); err != nil {
		t.Fatalf("cli write: %v", err)
	}
	if err := cli.Close(); err != nil {
		t.Fatalf("cli close: %v", err)
	}

	// The daemon's handle must still see the CLI's write, and vice versa —
	// no split-brain.
	if v, _ := daemon.Setting(ctx, "cli_probe"); v != "from-cli" {
		t.Fatalf("daemon cannot see cli write: got %q", v)
	}

	// Second process returns: its write must still be visible.
	cli2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer cli2.Close()
	if v, _ := cli2.Setting(ctx, "probe"); v != "daemon" {
		t.Fatalf("reopened store cannot see daemon write: got %q", v)
	}
	assertNoSidecars(t, dir)
}

func assertNoSidecars(t *testing.T, dir string) {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(filepath.Join(dir, "data.db"+suffix)); err == nil {
			t.Fatalf("unexpected sidecar file data.db%s exists in DELETE mode", suffix)
		}
	}
}
