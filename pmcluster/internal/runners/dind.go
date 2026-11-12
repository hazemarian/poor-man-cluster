package runners

import (
	"context"
	"fmt"
	"time"
)

// DinD lifecycle: two containers and one volume per runner, in a fixed order.
//
//	dind (privileged, own daemon)  ← created first, waited for
//	  └ shared socket volume
//	runner                         ← created only once the daemon answers
//
// Teardown is the reverse, and it removes the volume too: a job's images, its
// containers and its build cache all live inside that volume, so leaving it behind
// would mean a node that fills up slowly with the debris of finished jobs.
//
// Every step is idempotent. A crashed job leaves the dind container and the volume
// behind, and the next reconcile must be able to clear them without an operator.

// StartRunner starts this node's runner, creating its private daemon first when the
// spec asks for one. It returns the error of the first step that fails; the caller
// records it and the reconcile retries on the next tick.
func StartRunner(ctx context.Context, d LocalDocker, s Spec) error {
	if !s.DinD {
		argv, err := s.Argv()
		if err != nil {
			return err
		}
		return d.Run(ctx, argv)
	}

	// Clear anything a previous job left. The volume especially: a stale copy holds
	// an inner daemon's /var/run, including a socket path that no process owns, and
	// the new dockerd would have to fight it for the same path.
	if err := StopRunner(ctx, d, s.RunnerName); err != nil {
		return fmt.Errorf("clear the previous dind runner %s: %w", s.RunnerName, err)
	}
	dindArgv, err := s.DinDArgv()
	if err != nil {
		return err
	}
	if err := d.Run(ctx, dindArgv); err != nil {
		return fmt.Errorf("start the dind container %s: %w", s.DinDName(), err)
	}
	// A container that is "running" is not a daemon that is listening: dockerd
	// inside the container starts a moment later, so the runner is gated on the
	// inner daemon answering.
	if err := waitForDaemon(ctx, d, s.DinDName()); err != nil {
		_ = StopRunner(ctx, d, s.RunnerName)
		return fmt.Errorf("the dind daemon in %s never answered: %w", s.DinDName(), err)
	}
	argv, err := s.Argv()
	if err != nil {
		_ = StopRunner(ctx, d, s.RunnerName)
		return err
	}
	if err := d.Run(ctx, argv); err != nil {
		// Do not leave a privileged daemon running with no runner attached: it would
		// hold the volume and consume the node for nothing.
		_ = StopRunner(ctx, d, s.RunnerName)
		return fmt.Errorf("start the runner container %s: %w", s.RunnerName, err)
	}
	return nil
}

// StopRunner removes the runner, its dind container and their socket volume, in
// that order — containers first, because a volume still in use cannot be removed.
// Names are derived from the runner name, so this works on a node that has no
// assignment and on a name whose containers were never created.
func StopRunner(ctx context.Context, d LocalDocker, runnerName string) error {
	if runnerName == "" {
		return nil
	}
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	keep(d.Remove(ctx, runnerName))
	keep(d.Remove(ctx, runnerName+"-dind"))
	keep(d.RemoveVolume(ctx, runnerName+"-docker"))
	return firstErr
}

// waitForDaemon polls the inner daemon until `docker info` succeeds.
//
// 60 seconds because a first boot on a small node copies the image and initialises
// /var/lib/docker; the caller's context bounds it further.
// dindWaitTimeout is a variable so a test does not spend a real minute proving that
// a daemon which never answers is refused.
var dindWaitTimeout = 60 * time.Second

func waitForDaemon(ctx context.Context, d LocalDocker, name string) error {
	deadline := time.Now().Add(dindWaitTimeout)
	var last error
	for {
		if _, err := d.ExecIn(ctx, name, "docker", "info"); err == nil {
			return nil
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			if last == nil {
				last = fmt.Errorf("no attempt was made")
			}
			return fmt.Errorf("no daemon after 60s: %w", last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
