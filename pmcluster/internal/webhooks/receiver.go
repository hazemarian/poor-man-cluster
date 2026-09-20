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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
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
type Receiver struct {
	Sources SourceReader
	Deploy  Deployer
	// Record receives every delivery outcome. Optional (nil disables history)
	// and best-effort: a recording failure must never change the response.
	Record Recorder
}

// Mount registers POST /webhook/{source}. Caller MUST place this outside
// the Bearer-protected /api subtree — HMAC IS the auth.
func (h *Receiver) Mount(r chi.Router) {
	r.Post("/webhook/{source}", h.receive)
}

// instruments is lazily-built so importing this package doesn't bind to
// the noop MeterProvider before telemetry.Init runs.
var (
	instrOnce       sync.Once
	webhookRequests metric.Int64Counter
)

func webhookCounter() metric.Int64Counter {
	instrOnce.Do(func() {
		meter := otel.Meter("github.com/hazemarian/poor-man-cluster/pmcluster/internal/webhooks")
		var err error
		webhookRequests, err = meter.Int64Counter(
			"pmcluster.webhook.requests.total",
			metric.WithDescription("Webhook receiver outcomes. Status is one of accepted|unauthorized|bad_request|server_error — never per-failure-mode to preserve the 401 indistinguishability invariant."),
		)
		if err != nil {
			webhookRequests, _ = otel.Meter("noop").Int64Counter("noop")
		}
	})
	return webhookRequests
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
//   - 502: docker stack deploy returned an error.
func (h *Receiver) receive(w http.ResponseWriter, r *http.Request) {
	source := chi.URLParam(r, "source")

	record := func(status string) {
		webhookCounter().Add(r.Context(), 1,
			metric.WithAttributes(
				attribute.String("source", source),
				attribute.String("status", status),
			),
		)
	}

	// recordDelivery persists one outcome to delivery history when a recorder
	// is wired. Best-effort by contract — never fails the request.
	recordDelivery := func(status string, p *stacks.Payload, deployErr error, res *stacks.Result) {
		if h.Record == nil {
			return
		}
		d := &Delivery{Source: source, Status: status}
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
		_ = h.Record.Record(r.Context(), d)
	}

	if source == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source required"})
		record("bad_request")
		recordDelivery("bad_request", nil, nil, nil)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		record("bad_request")
		recordDelivery("bad_request", nil, nil, nil)
		return
	}
	if len(body) > MaxBodyBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "body too large"})
		record("bad_request")
		recordDelivery("bad_request", nil, nil, nil)
		return
	}

	timestamp, tsErr := parseTimestamp(r.Header.Get(TimestampHeader))

	if err := h.verifyHMAC(r.Context(), source, timestamp, body, r.Header.Get(SignatureHeader), tsErr); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		record("unauthorized")
		recordDelivery("unauthorized", nil, nil, nil)
		return
	}

	_ = h.Sources.MarkUsed(r.Context(), source)

	var p stacks.Payload
	if err := json.Unmarshal(body, &p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		record("bad_request")
		recordDelivery("bad_request", nil, nil, nil)
		return
	}

	// Webhook deploys must carry provenance: which repo + which manifest file
	// produced this deploy. Without them a revision cannot be traced back to
	// its source (and the console cannot offer a git-backed sync later).
	if p.RepoURL == "" || p.File == "" {
		provErr := fmt.Errorf("deploy provenance required: 'repo_url' and 'file' must identify the source repository and manifest path")
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": provErr.Error()})
		record("bad_request")
		recordDelivery("bad_request", &p, provErr, nil)
		return
	}

	res, err := h.Deploy.Deploy(r.Context(), p)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		record("server_error")
		recordDelivery("server_error", &p, err, nil)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"stack":    res.StackName,
		"revision": res.Revision,
	})
	record("accepted")
	recordDelivery("accepted", &p, nil, res)
}

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
