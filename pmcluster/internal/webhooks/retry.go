package webhooks

import (
	"context"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
)

// Deploy-retry policy for webhook-triggered deploys.
//
// A webhook deploy that fails with a server_error is retried: deploys are
// usually idempotent (the payload is re-rendered and re-applied), and most
// webhook failures are transient (swarm node busy, registry hiccup, a
// momentary docker API error). The retry count is recorded on the delivery
// row so history shows whether a failure died on the first try or only
// after the whole budget was spent.
const (
	// DefaultDeployRetries is how many extra attempts a failed webhook
	// deploy earns after the first one.
	DefaultDeployRetries = 2

	// DefaultDeployRetryDelay is the pause between attempts.
	DefaultDeployRetryDelay = 30 * time.Second

	// deployAttemptBudget is how long a single deploy attempt may run
	// inside the retry phase.
	deployAttemptBudget = 60 * time.Second
)

// deployPhaseBudget bounds the whole deploy + retry phase: every attempt
// gets deployAttemptBudget, plus the waits in between. The default policy
// (2 retries × 30s apart + 3 attempts) lands on 4 minutes.
//
// It exists because the phase deliberately outlives the request's own
// deadline: the router cancels every request 30s after it arrives (chi's
// middleware.Timeout), which is shorter than the retry window itself — the
// first 30s wait alone would never finish. The deploy + retry therefore
// runs on a detached context carrying this budget (see receive), so the
// retry sequence completes and the delivery row records the real outcome
// even if the caller stopped waiting. Detaching also means a CI client
// that hangs up does not abort a deploy already in flight: the delivery
// history, not the response, is the record of record.
func deployPhaseBudget(attempts int, delay time.Duration) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if delay < 0 {
		delay = 0
	}
	return time.Duration(attempts+1)*deployAttemptBudget + time.Duration(attempts)*delay
}

// RetryDeployer wraps a Deployer with the bounded retry policy above: on a
// failed Deploy it retries up to Attempts more times, Delay apart. It is
// itself a Deployer (Deploy simply drops the retry count); callers that
// need the count — the receiver records it on the delivery row — call
// DeployWithRetries.
type RetryDeployer struct {
	Inner Deployer
	// Attempts is the number of extra attempts after the first failure.
	// Negative disables retries.
	Attempts int
	// Delay is the pause between attempts. Zero (or negative) means no wait.
	Delay time.Duration
}

// Deploy satisfies Deployer, discarding how many retries were used.
func (r *RetryDeployer) Deploy(ctx context.Context, p stacks.Payload) (*stacks.Result, error) {
	res, _, err := r.DeployWithRetries(ctx, p)
	return res, err
}

// DeployAsync satisfies Deployer for the fire-and-forget path: validation is
// synchronous, the swarm apply is backgrounded by the inner Deployer.
func (r *RetryDeployer) DeployAsync(ctx context.Context, p stacks.Payload) (*stacks.Result, error) {
	return r.Inner.DeployAsync(ctx, p)
}

// DeployWithRetries runs the underlying Deployer under the retry policy and
// also reports the number of retries actually performed (0..Attempts).
func (r *RetryDeployer) DeployWithRetries(ctx context.Context, p stacks.Payload) (*stacks.Result, int, error) {
	return retryDeploy(ctx, r.Inner, p, r.Attempts, r.Delay)
}

// retryDeploy is the unit-testable core of the retry policy: it runs
// dep.Deploy once and, while that attempt fails, repeats it up to `attempts`
// more times, sleeping `delay` between attempts. It returns the final
// result, the number of retries actually performed (0..attempts), and the
// final error (nil when an attempt succeeded).
//
// The wait between attempts selects on ctx.Done(), so a canceled request
// stops the loop immediately: the ORIGINAL deploy error is returned rather
// than the context error, keeping the recorded delivery message useful.
// Any deploy error is retried — distinguishing transient from permanent
// failures is not possible at this layer (permanent failures such as a
// manifest conflict simply fail identically on every attempt).
func retryDeploy(ctx context.Context, dep Deployer, p stacks.Payload, attempts int, delay time.Duration) (*stacks.Result, int, error) {
	if attempts < 0 {
		attempts = 0
	}
	var (
		res     *stacks.Result
		err     error
		retries int
	)
	for {
		res, err = dep.Deploy(ctx, p)
		if err == nil || retries >= attempts {
			return res, retries, err
		}
		if !waitBeforeRetry(ctx, delay) {
			return res, retries, err
		}
		retries++
	}
}

// waitBeforeRetry sleeps delay and reports whether another attempt should
// run. It returns false as soon as ctx is done (canceled, or past the
// deploy-phase budget — see deployPhaseBudget), leaving the remaining
// retries unspent instead of burning them against a dead context.
func waitBeforeRetry(ctx context.Context, delay time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if delay <= 0 {
		return true
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// retryer builds the policy for one request. The zero value of the receiver
// fields falls back to the package defaults above (production behaviour);
// set MaxRetries to a negative value to disable retries.
func (h *Receiver) retryer() *RetryDeployer {
	attempts := h.MaxRetries
	if attempts == 0 {
		attempts = DefaultDeployRetries
	}
	delay := h.RetryDelay
	if delay == 0 {
		delay = DefaultDeployRetryDelay
	}
	return &RetryDeployer{Inner: h.Deploy, Attempts: attempts, Delay: delay}
}
