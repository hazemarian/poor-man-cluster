package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/telemetry"
)

// Receiver is the HMAC-verified deploy webhook receiver.
//
// Endpoint: POST /webhook/{source}
//
//	Body:     stacks.Payload as JSON
//	Header:   X-Pmcluster-Signature: sha256=<hex>
//	Header:   X-Pmcluster-Timestamp: <unix-seconds>    (REQUIRED for replay protection)
//
// The signature is HMAC-SHA256 over (timestamp + body) with the shared
// secret stored under the named source.  Constant-time comparison on the
// hex-decoded digest.  Requests older than 5 minutes are rejected.
// The endpoint is unauthenticated (no Bearer token); the HMAC IS the auth.
// CI systems can post freely as long as they hold the source's shared secret.
//
// A deploy that fails is retried (see retry.go: default 2 extra attempts,
// 30s apart) before the response is written; the delivery row records how
// many retries were spent — see MaxRetries/RetryDelay to tune or disable.
type Receiver struct {
	Sources SourceReader
	Deploy  Deployer
	// Record receives every delivery outcome. Optional (nil disables history)
	// and best-effort: a recording failure must never change the response.
	Record Recorder

	// MaxRetries is how many extra deploy attempts a failed deploy earns
	// before the receiver gives up. Zero means DefaultDeployRetries (2);
	// a negative value disables retries entirely.
	MaxRetries int
	// RetryDelay is the pause between attempts. Zero means
	// DefaultDeployRetryDelay (30s).
	RetryDelay time.Duration

	// inflight counts the goroutines a request is allowed to leave behind: the
	// retry phase, and the 202 follow-up that records its outcome. Nothing in
	// production waits on it — the process is long-lived — but a *test* does:
	// the delivery row is written after the response, so a test that returns
	// first closes the store (and deletes its temp dir) under a live writer.
	// That is a flake with no failing assertion, which is the worst kind.
	inflight sync.WaitGroup
}

// Wait blocks until the goroutines started by in-flight requests have finished.
// Only tests and shutdown call it; a request handler never does.
func (h *Receiver) Wait() { h.inflight.Wait() }

// Mount registers POST /webhook/{source}. Caller MUST place this outside
// the Bearer-protected /api subtree — HMAC IS the auth.
func (h *Receiver) Mount(r chi.Router) {
	r.Post("/webhook/{source}", h.receive)
}

// SignatureHeader format: "sha256=<lowercase-hex>". Matches GitHub/GitLab
// so off-the-shelf CI integrations work.
const SignatureHeader = "X-Pmcluster-Signature"

// TimestampHeader carries the unix-seconds timestamp of the request.
// The HMAC is computed over timestamp_seconds + body to prevent replays.
const TimestampHeader = "X-Pmcluster-Timestamp"

// MaxClockSkew is how far a timestamp may drift from the server's clock.
// 5 minutes is generous for NTP-disciplined CI runners.
const MaxClockSkew = 5 * time.Minute

// MaxBodyBytes caps the webhook body. Manifests are <10 KB typically;
// 1 MB leaves headroom and bounds HMAC compute cost from hostile callers.
const MaxBodyBytes = 1 << 20

// HTTP status discipline:
//   - 401: any HMAC failure mode (missing/invalid sig, bad timestamp, unknown source).
//     Same status for all so an attacker can't distinguish the cases.
//   - 400: body too large, malformed JSON, or deploy validation failure.
//   - 502: docker stack deploy returned an error after the retry budget
//     (MaxRetries) was spent; the body carries the retry count.
func (h *Receiver) receive(w http.ResponseWriter, r *http.Request) {
	source := chi.URLParam(r, "source")

	// record emits the pmcluster.webhook.requests.total counter for this
	// outcome (Q5 alerting metric). It fires on EVERY exit path — success
	// and failure alike, history or not — and runs before the response is
	// written so the counter never lags the observable outcome. Best-effort
	// by contract: a metric problem must never change the response.
	record := func(status string) {
		telemetry.RecordWebhookDelivery(r.Context(), source, status)
	}

	// recordDelivery persists one outcome to delivery history when a recorder
	// is wired. Best-effort by contract — never fails the request. retries is
	// how many extra deploy attempts were made before this outcome (always 0
	// for outcomes decided before the deploy step).
	//
	// The history write runs on a context detached from the request: the
	// retry phase can outlast the router's per-request timeout (see
	// deployPhaseBudget), and a caller that hung up must not cost us the
	// delivery row. Trace values are kept; only cancellation is dropped.
	recordDelivery := func(status string, p *stacks.Payload, deployErr error, res *stacks.Result, retries int) {
		if h.Record == nil {
			return
		}
		d := &Delivery{Source: source, Status: status, Retries: retries}
		if p != nil {
			d.StackName = p.AppName
			d.RepoURL = p.RepoURL
			d.File = p.File
		}
		if res != nil {
			d.StackName = res.StackName
			d.Revision = res.Revision
		}
		if deployErr != nil {
			d.Error = deployErr.Error()
		}
		_ = h.Record.Record(context.WithoutCancel(r.Context()), d)
	}

	if source == "" {
		record("bad_request")
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source required"})
		recordDelivery("bad_request", nil, nil, nil, 0)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		record("bad_request")
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		recordDelivery("bad_request", nil, nil, nil, 0)
		return
	}
	if len(body) > MaxBodyBytes {
		record("bad_request")
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "body too large"})
		recordDelivery("bad_request", nil, nil, nil, 0)
		return
	}

	timestamp, tsErr := parseTimestamp(r.Header.Get(TimestampHeader))

	if err := h.verifyHMAC(r.Context(), source, timestamp, body, r.Header.Get(SignatureHeader), tsErr); err != nil {
		record("unauthorized")
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		recordDelivery("unauthorized", nil, nil, nil, 0)
		return
	}

	_ = h.Sources.MarkUsed(r.Context(), source)

	var p stacks.Payload
	if err := json.Unmarshal(body, &p); err != nil {
		record("bad_request")
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		recordDelivery("bad_request", nil, nil, nil, 0)
		return
	}

	// Webhook deploys must carry provenance: which repo + which manifest file
	// produced this deploy. Without them a revision cannot be traced back to
	// its source (and the console cannot offer a git-backed sync later).
	if p.RepoURL == "" || p.File == "" {
		provErr := fmt.Errorf("deploy provenance required: 'repo_url' and 'file' must identify the source repository and manifest path")
		record("bad_request")
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": provErr.Error()})
		recordDelivery("bad_request", &p, provErr, nil, 0)
		return
	}

	// Fire-and-forget with retry: validation, provenance and revision
	// recording are all synchronous in DeployAsync, so any validation error
	// still surfaces here (400/502). Those synchronous failures are retried
	// under the bounded retry policy. Only the swarm apply — and its per-level
	// health waits — runs in the background, detached from the request
	// deadline.
	//
	// The deploy + retry phase runs on a detached context carrying the phase
	// budget: the retry window (default 2 × 30s + 3 attempts × 60s ≈ 4min)
	// deliberately outlives the router's per-request timeout, so the retry
	// sequence completes and the delivery row records the real outcome even
	// if the caller stopped waiting.
	policy := h.retryer()
	phaseCtx, cancelPhase := context.WithTimeout(context.WithoutCancel(r.Context()), deployPhaseBudget(policy.Attempts, policy.Delay))
	// Deliberately NOT deferred.
	//
	// The 202 below answers "accepted, still running" and leaves the retry phase
	// going; cancelling it when this function returns would abort the very work
	// that answer promised, and — worse — a cancelled deploy surfaces as "no
	// error", so the delivery row the follow-up goroutine writes would say
	// "accepted" for a deploy that was killed mid-flight. Ownership of this
	// cancel is transferred to that goroutine on the 202 path; the phase deadline
	// set above still bounds the work either way.

	// The retry phase runs on its own goroutine from here: a retryable failure
	// can spend minutes of sleeps, and the caller is waiting on an HTTP request
	// with a 30s ceiling in front of it (the edge proxy). Answering only after
	// the phase finishes is what turned a missing secret into
	// {"error":"control plane unavailable"} — the proxy timed out on a deploy
	// the receiver already knew had failed.
	type outcome struct {
		res     *stacks.Result
		retries int
		err     error
	}
	phase := make(chan outcome, 1)
	h.inflight.Add(1)
	go func() {
		defer h.inflight.Done()
		res, retries, err := policy.DeployAsyncWithRetries(phaseCtx, p)
		phase <- outcome{res: res, retries: retries, err: err}
	}()

	var out outcome
	select {
	case out = <-phase:
		// The phase is over: releasing its context now frees the deadline timer.
		cancelPhase()
	case <-time.After(webhookResponseBudget):
		// Still retrying. Answer now with what the caller can act on, and let
		// the delivery row — which the goroutine records — carry the outcome.
		// This is the honest split: the HTTP response is about this request,
		// the delivery history is about the deploy.
		h.inflight.Add(1)
		go func() {
			// This goroutine owns the phase from here (see the note where the
			// context is created), so it is also the one that releases it.
			defer h.inflight.Done()
			defer cancelPhase()
			o := <-phase
			status, deployErr := "accepted", error(nil)
			if o.err != nil {
				status, deployErr = "server_error", o.err
			}
			recordDelivery(status, &p, deployErr, o.res, o.retries)
		}()
		record("accepted_pending")
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status":  "accepted",
			"pending": true,
			"note": "the deploy and its retries are still running; the delivery history records the outcome " +
				"(`pmcluster webhook deliveries " + source + "`)",
			"retry_window": deployPhaseBudget(policy.Attempts, policy.Delay).String(),
		})
		return
	}

	if out.err != nil {
		if step, deterministic := Deterministic(out.err); deterministic {
			// Nothing about this payload can succeed on a retry: name the cause
			// and the step instead of spending the retry window.
			record("rejected")
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":  out.err.Error(),
				"step":   step,
				"status": "rejected",
				"help":   "this deploy will fail identically until the payload or the cluster state changes",
			})
			recordDelivery("rejected", &p, out.err, nil, 0)
			return
		}
		record("server_error")
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": out.err.Error(), "retries": out.retries})
		recordDelivery("server_error", &p, out.err, nil, out.retries)
		return
	}

	record("accepted")
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":   "accepted",
		"stack":    out.res.StackName,
		"revision": out.res.Revision,
	})
	recordDelivery("accepted", &p, nil, out.res, out.retries)
}

// webhookResponseBudget bounds how long the HTTP response waits for the deploy
// phase before answering 202 and leaving the rest to the delivery history.
//
// It must stay under the ingress ceiling: the edge proxy in front of the daemon
// answers its own 30s REQUEST_TIMEOUT first, which is how a deterministic deploy
// failure was reported to CI as `{"error":"control plane unavailable"}`.
// It is a variable so tests can shrink it; production never writes to it.
var webhookResponseBudget = 20 * time.Second

// parseTimestamp returns the unix-seconds value.  If the header is empty
// or unparseable, tsErr is non-nil and verifyHMAC handles it uniformly.
func parseTimestamp(header string) (int64, error) {
	if header == "" {
		return 0, errors.New("missing timestamp header")
	}
	v, err := strconv.ParseInt(header, 10, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("invalid timestamp: %q", header)
	}
	return v, nil
}

// verifyHMAC returns a non-nil error for ANY failure mode. Caller maps
// every error to a single 401 to avoid leaking auth state.
//
// Replay protection: the HMAC is computed over
//
//	timestamp_as_decimal_string + body
//
// and the timestamp must be within MaxClockSkew of the server's clock.
func (h *Receiver) verifyHMAC(ctx context.Context, source string, timestamp int64, body []byte, sigHeader string, tsErr error) error {

	if tsErr != nil {
		return tsErr
	}

	now := time.Now().Unix()
	delta := now - timestamp
	if delta < 0 {
		delta = -delta
	}
	if delta > int64(MaxClockSkew.Seconds()) {
		return fmt.Errorf("timestamp skew too large: %d seconds", delta)
	}

	if sigHeader == "" {
		return errors.New("missing signature header")
	}
	parsed, ok := strings.CutPrefix(sigHeader, "sha256=")
	if !ok {
		return errors.New("signature must be sha256=<hex>")
	}
	want, err := hex.DecodeString(parsed)
	if err != nil || len(want) != sha256.Size {
		return errors.New("signature must be 64 hex chars after sha256=")
	}

	secret, err := h.Sources.Secret(ctx, source)
	if err != nil {
		return err
	}

	mac := hmac.New(sha256.New, secret)
	fmt.Fprint(mac, timestamp)
	mac.Write(body)
	got := mac.Sum(nil)

	if !hmac.Equal(want, got) {
		return errors.New("signature mismatch")
	}
	return nil
}
