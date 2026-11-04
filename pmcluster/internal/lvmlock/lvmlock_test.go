package lvmlock

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The regression this package exists for: the daemon's `vgs` poll and a
// `storage grow` deadlocked on production, and lvextend slept twelve minutes while
// every other LVM command queued behind it.
func TestWaitsForAConcurrentCommandAndThenProceeds(t *testing.T) {
	Path = filepath.Join(t.TempDir(), "pmcluster.lock")

	release := make(chan struct{})
	held := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = With(context.Background(), func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	// A second command must wait — but not forever.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	err := With(ctx, func() error { return nil })
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v, want ErrBusy: a blocked probe must give up, not hang", err)
	}

	close(release)
	wg.Wait()

	if err := With(context.Background(), func() error { return nil }); err != nil {
		t.Fatalf("lock not released after the holder finished: %v", err)
	}
}

// fn's error must survive the wrapper: callers distinguish "LVM failed" from
// "LVM was busy".
func TestTheCommandErrorIsNotSwallowed(t *testing.T) {
	Path = filepath.Join(t.TempDir(), "pmcluster.lock")
	want := errors.New("lvcreate exploded")
	if err := With(context.Background(), func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}
