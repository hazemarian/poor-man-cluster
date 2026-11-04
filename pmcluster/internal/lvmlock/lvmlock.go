// Package lvmlock serialises every LVM command this program runs.
//
// LVM has two locks that different commands take in different orders: a global
// lock and a per-volume-group lock. `vgs` — the read-only probe the daemon's
// health loop runs — takes the global lock and then waits for the group lock,
// while `lvextend` takes the group lock and then waits for the global one. Run at
// the wrong moment they deadlock.
//
// This is not theoretical. On a production node a `storage grow` deadlocked with
// the daemon's poll: `lvextend` slept for twelve minutes holding vg-pmcluster
// while every `lvs`, `vgs` and login-time LVM probe queued behind it. The node was
// not down — Swarm still reported it Ready and its services kept running — but no
// command that touched LVM could make progress, which looks exactly like a hung
// machine from the outside.
//
// LVM cannot be told to sort its own locks out, so this program serialises its
// own calls: one exclusive flock around every LVM invocation, reads included. The
// lock is held for as long as the command runs, so callers must pass a context
// with a deadline; waiting forever is the failure this package exists to remove.
package lvmlock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// Path is the advisory lock every LVM command in this program takes. It lives
// beside LVM's own locks so an operator looking for "what is holding LVM" finds it
// in the obvious place. It is a variable only so tests can point it elsewhere.
var Path = "/run/lock/lvm/pmcluster.lock"

// ErrBusy means another LVM command held the lock past the caller's deadline.
var ErrBusy = errors.New("another LVM command holds the lock")

// With runs fn while holding the LVM lock. The lock is released even if fn panics.
//
// A locked body must call the package's unlocked internals (`detect`, `grow`,
// `provision`), never its exported wrappers: flock conflicts with itself, so a
// second acquire from the same process parks forever. Grow did exactly that and
// hung a production node's LVM for seven minutes.
//
// A short poll interval is used rather than a blocking flock so the caller's
// context is honoured: a daemon probe waiting on a twenty-minute `lvextend` must
// give up and report "storage busy", not hang the whole daemon.
func With(ctx context.Context, fn func() error) error {
	f, err := os.OpenFile(Path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		// The lock is an optimisation for safety, not a dependency: if the path
		// does not exist (a container without /run/lock), run the command anyway
		// rather than refusing to manage storage.
		return fn()
	}
	defer f.Close()

	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("lock %s: %w", Path, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %s", ErrBusy, Path)
		case <-time.After(200 * time.Millisecond):
		}
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	return fn()
}
