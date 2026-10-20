package server

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLogReaderTailScansNewestFirstAndReportsRowCut(t *testing.T) {
	dir := t.TempDir()
	writeLogFixture(t, dir, "pmcluster-20261018.log", "old-1\nold-2\n")
	writeLogFixture(t, dir, "pmcluster-20261019.log", "new-1\nnew-2\n")

	page, err := (&LogReader{Dir: dir}).Tail(context.Background(), 2, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(page.Lines, []string{"new-1", "new-2"}) {
		t.Fatalf("lines = %#v", page.Lines)
	}
	if !page.Truncated || !reflect.DeepEqual(page.TruncationReasons, []string{"row_limit"}) {
		t.Fatalf("truncation = %v reasons = %#v", page.Truncated, page.TruncationReasons)
	}
}

func TestLogReaderTailReportsFileLimit(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < logAllFileMax+1; i++ {
		name := filepath.Join(dir, "pmcluster-2026-"+strings.Repeat("0", 2)+string(rune('a'+i/26))+string(rune('a'+i%26))+".log")
		writeLogFixture(t, dir, filepath.Base(name), "line\n")
	}

	page, err := (&LogReader{Dir: dir}).Tail(context.Background(), logAllFileMax+1, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != logAllFileMax || !page.Truncated || !reflect.DeepEqual(page.TruncationReasons, []string{"file_limit"}) {
		t.Fatalf("lines=%d truncated=%v reasons=%#v", len(page.Lines), page.Truncated, page.TruncationReasons)
	}
}

func TestLogReaderTailHonorsCancellation(t *testing.T) {
	dir := t.TempDir()
	writeLogFixture(t, dir, "pmcluster-20261019.log", "line\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (&LogReader{Dir: dir}).Tail(ctx, 1, 0, false)
	if err == nil {
		t.Fatal("expected cancellation")
	}
}

func writeLogFixture(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
