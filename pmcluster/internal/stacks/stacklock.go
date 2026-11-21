package stacks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Deploys of one stack are serialized ACROSS PROCESSES.
//
// The daemon's reconcile loop and an operator's `pmcluster deploy` are different processes writing to
// the same swarm. Their docker calls interleave: `docker stack deploy` lists a stack's services and
// then creates the ones it did not see, so a service created in between makes the create fail with
// "name conflicts with an existing object" for a service that exists (retried since v0.2.389), and two
// updates of one service inside Swarm's version window answer "update out of sequence".
//
// Those retries make the platform SURVIVE the race. This lock stops the race happening: one deploy of
// a stack at a time, whoever asks for it.
//
// It is a file lock, not a mutex, because the contenders are processes. It is taken at the ENTRY of a
// deploy — before the revision and the render are read — not around the docker call: a lock taken only
// for the apply still lets the reconcile decide "needs deploying" against state that an operator's
// deploy has already replaced, and then apply the older render over it.
// lockDir is <data dir>/locks: next to the store, so the daemon and the CLI agree on the path without
// a setting to forget. A service with no store (unit tests, render-only callers) does not lock.
func (s *Service) lockDir() string {
	if s.Store == nil {
		return ""
	}
	p := s.Store.Path()
	if p == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p), "locks")
}

// withStackLock runs fn holding the exclusive deploy lock for stack, waiting at most deployWaitTimeout
// for the current holder.
//
// The lock file carries the holder's pid and the time it was taken, so a wait that times out names
// who to look at instead of saying "busy": a silent five-minute hang is worse than the race it
// replaces.
// heldStacksKey marks, in a context, the stacks whose deploy lock THIS call chain already holds.
//
// It exists because the pipeline nests: Sync re-applies a revision by calling the deploy path, so a
// lock taken at both entries deadlocks against itself. flock is per open file description, and a second
// open in the same process conflicts with the first exactly as another process would — which is how the
// nesting announced itself: `withStackLock → deploy → withStackLock`, waiting on its own lock.
type heldStacksKey struct{}

func withHeldStack(ctx context.Context, stack string) context.Context {
	held, _ := ctx.Value(heldStacksKey{}).(map[string]bool)
	next := make(map[string]bool, len(held)+1)
	for k := range held {
		next[k] = true
	}
	next[stack] = true
	return context.WithValue(ctx, heldStacksKey{}, next)
}

func holdsStack(ctx context.Context, stack string) bool {
	held, _ := ctx.Value(heldStacksKey{}).(map[string]bool)
	return held[stack]
}

func (s *Service) withStackLock(ctx context.Context, stack string, fn func(context.Context) error) error {
	// Already held further up this call chain: the nested step runs inside the outer lock.
	if holdsStack(ctx, stack) {
		return fn(ctx)
	}
	dir := s.lockDir()
	if dir == "" || stack == "" {
		return fn(ctx)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.Log.Warn().Err(err).Str("stack", stack).Msg("stack lock: cannot create the lock directory — deploying unserialized")
		return fn(ctx)
	}
	path := filepath.Join(dir, "stack-"+sanitizeLockName(stack)+".lock")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		s.Log.Warn().Err(err).Str("stack", stack).Msg("stack lock: cannot open the lock file — deploying unserialized")
		return fn(ctx)
	}
	defer f.Close()

	deadline := time.Now().Add(stackLockWaitTimeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EINVAL) {
			// A filesystem without advisory locks. Today's behaviour (racing) is better than a
			// platform where every deploy fails for a reason no operator can act on.
			s.Log.Warn().Str("stack", stack).Str("dir", dir).
				Msg("stack lock: this filesystem has no advisory locks — deploys of this stack are not serialized")
			return fn(ctx)
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			s.Log.Warn().Err(err).Str("stack", stack).Msg("stack lock: flock failed — deploying unserialized")
			return fn(ctx)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("another deploy of %q is in progress%s and did not finish within %s; "+
				"wait for it, or remove the lock file if that process is gone",
				stack, s.lockHolder(f), stackLockWaitTimeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for another deploy of %q: %w", stack, ctx.Err())
		case <-time.After(stackLockPollInterval):
		}
	}

	// Held. Record who holds it, for the next caller's timeout message.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(fmt.Sprintf("pid=%d at=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))), 0)
	s.Log.Debug().Str("stack", stack).Str("lock", path).Msg("stack lock: acquired")

	defer func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Truncate(0) // a released lock must not look held
		s.Log.Debug().Str("stack", stack).Msg("stack lock: released")
	}()
	return fn(withHeldStack(ctx, stack))
}

// stackLockPollInterval is how often a waiting deploy retries the lock. Short enough that a normal
// hand-over costs nothing, long enough that a crowd of waiters is not a spin.
const stackLockPollInterval = 250 * time.Millisecond

// stackLockWaitTimeout is how long a waiter waits before giving up.
//
// Deliberately NOT deployWaitTimeout: a deploy holds this lock for its whole pipeline, including up to
// one health wait per depends_on level, so a three-level stack on a cold database can legitimately hold
// it for six minutes. A waiter that gave up at two would fail every time that stack deployed — and it
// is the reconcile that waits, so the failure would read as "the loop is broken". Waiting longer than a
// healthy deploy costs nothing; the timeout exists only so a stuck holder becomes a message.
var stackLockWaitTimeout = 5 * time.Minute

// lockHolder reads the current holder's note, for the message a timed-out waiter shows.
func (s *Service) lockHolder(f *os.File) string {
	buf := make([]byte, 128)
	n, _ := f.ReadAt(buf, 0)
	note := string(buf[:n])
	if note == "" {
		return ""
	}
	return " (" + trimNote(note) + ")"
}

func trimNote(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}

// sanitizeLockName keeps a stack name usable as a file name. Stack names are already [a-z0-9_-], so
// this is belt and braces for a name that reached here from a manifest field.
func sanitizeLockName(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
