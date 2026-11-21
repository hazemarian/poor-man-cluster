package stacks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
	"github.com/rs/zerolog"
)

// lockPair returns two Services whose stores live in ONE directory, so they derive the same lock dir —
// the relationship the daemon and the CLI have on a host. Two open file descriptions of one file are
// what flock sees for two processes, so this exercises the real mechanism rather than a goroutine stub.
func lockPair(t *testing.T) (a, b *Service) {
	t.Helper()
	dir := t.TempDir()
	open := func(name string) *store.Store {
		st, err := store.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("store.Open %s: %v", name, err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st
	}
	return &Service{Store: open("data.db"), Log: zerolog.Nop()},
		&Service{Store: open("data-b.db"), Log: zerolog.Nop()}
}

// TestWithStackLock_SerializesTwoHolders is the property that matters: while one deploy of a stack is
// running, a second must not proceed.
func TestWithStackLock_SerializesTwoHolders(t *testing.T) {
	a, b := lockPair(t)

	inside := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_ = a.withStackLock(context.Background(), "demo", func(context.Context) error {
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside // A holds the lock

	var bRan atomic.Bool
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		_ = b.withStackLock(context.Background(), "demo", func(context.Context) error {
			bRan.Store(true)
			return nil
		})
	}()

	time.Sleep(300 * time.Millisecond) // several poll intervals
	if bRan.Load() {
		t.Fatal("the second holder ran while the first still held the stack lock")
	}
	close(release)
	<-firstDone
	<-secondDone
	if !bRan.Load() {
		t.Fatal("the second holder never ran after the first released the lock")
	}
}

// TestWithStackLock_TimeoutNamesTheHolder — a lock that cannot be taken must not look like a hang: the
// error names the stack and the holder.
func TestWithStackLock_TimeoutNamesTheHolder(t *testing.T) {
	old := stackLockWaitTimeout
	stackLockWaitTimeout = 400 * time.Millisecond
	t.Cleanup(func() { stackLockWaitTimeout = old })

	a, b := lockPair(t)
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = a.withStackLock(context.Background(), "demo", func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	err := b.withStackLock(context.Background(), "demo", func(context.Context) error { return nil })
	close(release)
	if err == nil {
		t.Fatal("withStackLock waited instead of timing out")
	}
	msg := err.Error()
	if !strings.Contains(msg, `another deploy of "demo" is in progress`) {
		t.Errorf("the error does not name the stack: %v", err)
	}
	if !strings.Contains(msg, "pid=") {
		t.Errorf("the error does not name the holder: %v", err)
	}
}

// TestWithStackLock_ReleasedLockLooksFree — a released lock leaves no note that a later reader could
// mistake for a held one.
func TestWithStackLock_ReleasedLockLooksFree(t *testing.T) {
	s, _ := lockPair(t)
	if err := s.withStackLock(context.Background(), "demo", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("withStackLock: %v", err)
	}
	f, err := os.Open(filepath.Join(s.lockDir(), "stack-demo.lock"))
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer f.Close()
	buf := make([]byte, 64)
	n, _ := f.Read(buf)
	if strings.TrimSpace(string(buf[:n])) != "" {
		t.Errorf("a released lock still carries a holder note: %q", string(buf[:n]))
	}
}

// TestWithStackLock_ReentrantWithinOneCallChain — the pipeline nests (Sync re-applies through the
// deploy path), and flock is per file description, so the nested call conflicts with its own outer
// lock exactly as another process would. Without re-entrancy the whole suite hung: the trace read
// withStackLock → deploy → withStackLock, waiting on itself. The wait is shortened here so a
// regression fails in a second instead of hanging for five minutes.
func TestWithStackLock_ReentrantWithinOneCallChain(t *testing.T) {
	old := stackLockWaitTimeout
	stackLockWaitTimeout = 300 * time.Millisecond
	t.Cleanup(func() { stackLockWaitTimeout = old })

	s, _ := lockPair(t)
	innerRan := false
	err := s.withStackLock(context.Background(), "demo", func(ctx context.Context) error {
		return s.withStackLock(ctx, "demo", func(context.Context) error {
			innerRan = true
			return nil
		})
	})
	if err != nil {
		t.Fatalf("nested withStackLock: %v — a call chain re-entering its own lock must not wait", err)
	}
	if !innerRan {
		t.Fatal("the nested step never ran")
	}

	// A DIFFERENT stack in the same chain still locks normally.
	otherErr := s.withStackLock(context.Background(), "demo", func(ctx context.Context) error {
		return s.withStackLock(ctx, "other", func(context.Context) error { return nil })
	})
	if otherErr != nil {
		t.Fatalf("nested lock on a different stack: %v", otherErr)
	}
}

// TestWithStackLock_NoStoreRunsUnlocked — render-only callers have no store, so no shared lock dir to
// agree on. They must still work.
func TestWithStackLock_NoStoreRunsUnlocked(t *testing.T) {
	s := &Service{Log: zerolog.Nop()}
	ran := false
	if err := s.withStackLock(context.Background(), "demo", func(context.Context) error { ran = true; return nil }); err != nil {
		t.Fatalf("withStackLock: %v", err)
	}
	if !ran {
		t.Fatal("fn did not run without a store")
	}
}

// TestStackNameForLock — the key has to be known before the pipeline parses anything: the payload
// override wins, else the manifest's own `app:`.
func TestStackNameForLock(t *testing.T) {
	cases := []struct {
		p    Payload
		want string
	}{
		{Payload{AppName: "override", Manifest: "app: frommanifest\n"}, "override"},
		{Payload{Manifest: "app: frommanifest\nversion: v1\n"}, "frommanifest"},
		{Payload{Manifest: "not: a manifest\n"}, ""},
		{Payload{}, ""},
	}
	for _, tc := range cases {
		if got := stackNameForLock(tc.p); got != tc.want {
			t.Errorf("stackNameForLock(%q/%q) = %q, want %q", tc.p.Manifest, tc.p.AppName, got, tc.want)
		}
	}
}
