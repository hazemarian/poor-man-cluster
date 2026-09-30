package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// funcDeployer adapts a function to Deployer so retry tests can script
// per-attempt behaviour (fail, cancel the context, …) without a real stack.
type funcDeployer func(ctx context.Context, p stacks.Payload) (*stacks.Result, error)

func (f funcDeployer) Deploy(ctx context.Context, p stacks.Payload) (*stacks.Result, error) {
	return f(ctx, p)
}

// scriptedDeployer fails its first `failures` calls with err and succeeds
// afterwards. failures < 0 means every call fails. Safe for concurrent use
// (-race): the receiver test drives it from the server goroutine while the
// test body reads the call count.
type scriptedDeployer struct {
	mu       sync.Mutex
	failures int
	err      error
	calls    int
	result   stacks.Result
}

func (d *scriptedDeployer) Deploy(_ context.Context, _ stacks.Payload) (*stacks.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.err != nil && (d.failures < 0 || d.calls <= d.failures) {
		return nil, d.err
	}
	res := d.result
	return &res, nil
}

func (d *scriptedDeployer) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

const retryTestPayloadManifest = `app: whoami-webhook
env: production
domain: example.test
services:
  web:
    image: traefik/whoami:v1.10
    replicas: 1
    expose:
      port: 80
      host: web.whoami-webhook.example.test
`

func retryTestPayload() stacks.Payload {
	return stacks.Payload{
		Manifest: retryTestPayloadManifest,
		RepoURL:  "https://github.com/org/repo",
		File:     "deploy/app.yaml",
	}
}

// TestRetryDeploy_SucceedsAfterTransientFailures: two failures then a
// success — the wrapper spends both retries (delay injected as 1ms, so the
// test never sleeps the production 30s) and reports the count.
func TestRetryDeploy_SucceedsAfterTransientFailures(t *testing.T) {
	boom := errors.New("docker stack deploy failed")
	dep := &scriptedDeployer{
		failures: 2,
		err:      boom,
		result:   stacks.Result{StackName: "whoami-webhook", Revision: 7},
	}

	res, retries, err := retryDeploy(context.Background(), dep, retryTestPayload(), 2, time.Millisecond)
	if err != nil {
		t.Fatalf("retryDeploy: %v (want success on the 3rd attempt)", err)
	}
	if retries != 2 {
		t.Errorf("retries = %d, want 2", retries)
	}
	if res == nil || res.StackName != "whoami-webhook" || res.Revision != 7 {
		t.Errorf("result = %+v, want whoami-webhook rev 7", res)
	}
	if got := dep.callCount(); got != 3 {
		t.Errorf("deploy calls = %d, want 3 (1 + 2 retries)", got)
	}
}

// TestRetryDeploy_ExhaustsRetryBudget: every attempt fails — the final
// error (not a context error) is returned after exactly `attempts` retries.
func TestRetryDeploy_ExhaustsRetryBudget(t *testing.T) {
	boom := errors.New("docker stack deploy failed")
	dep := &scriptedDeployer{failures: -1, err: boom}

	res, retries, err := retryDeploy(context.Background(), dep, retryTestPayload(), 2, time.Millisecond)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the final deploy error", err)
	}
	if res != nil {
		t.Errorf("result = %+v, want nil", res)
	}
	if retries != 2 {
		t.Errorf("retries = %d, want 2 (budget exhausted)", retries)
	}
	if got := dep.callCount(); got != 3 {
		t.Errorf("deploy calls = %d, want 3", got)
	}
}

// TestRetryDeploy_DisabledAttempts: attempts <= 0 means exactly one call.
func TestRetryDeploy_DisabledAttempts(t *testing.T) {
	boom := errors.New("permanent failure")
	for _, attempts := range []int{0, -1} {
		dep := &scriptedDeployer{failures: -1, err: boom}
		_, retries, err := retryDeploy(context.Background(), dep, retryTestPayload(), attempts, time.Millisecond)
		if !errors.Is(err, boom) {
			t.Errorf("attempts=%d: err = %v, want deploy error", attempts, err)
		}
		if retries != 0 {
			t.Errorf("attempts=%d: retries = %d, want 0", attempts, retries)
		}
		if got := dep.callCount(); got != 1 {
			t.Errorf("attempts=%d: deploy calls = %d, want 1", attempts, got)
		}
	}
}

// TestRetryDeploy_StopsWhenContextDone: a canceled request ends the retry
// loop during the wait instead of sleeping the whole delay, and the
// recorded error stays the deploy error rather than the context error.
func TestRetryDeploy_StopsWhenContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	boom := errors.New("docker stack deploy failed")
	var calls int
	dep := funcDeployer(func(context.Context, stacks.Payload) (*stacks.Result, error) {
		calls++
		cancel() // the client goes away right after the first failed attempt
		return nil, boom
	})

	start := time.Now()
	_, retries, err := retryDeploy(ctx, dep, retryTestPayload(), 2, time.Hour)
	elapsed := time.Since(start)

	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the deploy error (not the context error)", err)
	}
	if retries != 0 {
		t.Errorf("retries = %d, want 0 (no retry after cancel)", retries)
	}
	if calls != 1 {
		t.Errorf("deploy calls = %d, want 1", calls)
	}
	if elapsed > 10*time.Second {
		t.Errorf("retryDeploy waited %s after cancel; the delay (1h) must be abandoned immediately", elapsed)
	}
}

// TestRetryDeployer_Wrapper covers the RetryDeployer wrapper itself: it
// behaves like a plain Deployer (the retry count is dropped) while still
// performing the retries underneath.
func TestRetryDeployer_WrapperDropsRetryCount(t *testing.T) {
	boom := errors.New("docker stack deploy failed")
	inner := &scriptedDeployer{
		failures: 1,
		err:      boom,
		result:   stacks.Result{StackName: "whoami-webhook", Revision: 3},
	}
	w := &RetryDeployer{Inner: inner, Attempts: 2, Delay: time.Millisecond}

	res, err := w.Deploy(context.Background(), retryTestPayload())
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res == nil || res.Revision != 3 {
		t.Errorf("result = %+v, want revision 3", res)
	}
	if got := inner.callCount(); got != 2 {
		t.Errorf("inner deploy calls = %d, want 2 (retry happened)", got)
	}

	res2, retries, err := w.DeployWithRetries(context.Background(), retryTestPayload())
	if err != nil || retries != 0 || res2 == nil {
		t.Errorf("second DeployWithRetries = (%+v, %d, %v), want success with 0 retries", res2, retries, err)
	}
}

// TestRetryerDefaults pins the zero-value policy: an unwired Receiver gets
// 2 extra attempts 30s apart (production default), and a negative
// MaxRetries turns retries off.
func TestRetryerDefaults(t *testing.T) {
	h := &Receiver{}
	r := h.retryer()
	if r.Attempts != DefaultDeployRetries {
		t.Errorf("default attempts = %d, want %d", r.Attempts, DefaultDeployRetries)
	}
	if r.Delay != DefaultDeployRetryDelay {
		t.Errorf("default delay = %s, want %s", r.Delay, DefaultDeployRetryDelay)
	}
	if DefaultDeployRetries != 2 || DefaultDeployRetryDelay != 30*time.Second {
		t.Errorf("policy drifted: %d retries / %s, want 2 / 30s", DefaultDeployRetries, DefaultDeployRetryDelay)
	}

	off := (&Receiver{MaxRetries: -1}).retryer()
	if off.Attempts >= 0 {
		t.Errorf("MaxRetries=-1 → attempts = %d, want negative (retries disabled)", off.Attempts)
	}
}

// TestDeployPhaseBudget pins the phase the retry loop runs under: it must
// cover every attempt plus every wait (so the router's 30s request timeout
// cannot cut the retry window short) while still bounding the handler.
func TestDeployPhaseBudget(t *testing.T) {
	got := deployPhaseBudget(DefaultDeployRetries, DefaultDeployRetryDelay)
	want := 3*deployAttemptBudget + 2*DefaultDeployRetryDelay
	if got != want {
		t.Errorf("default budget = %s, want %s", got, want)
	}
	if got <= 2*DefaultDeployRetryDelay {
		t.Errorf("budget %s does not exceed the retry window itself (%s)", got, 2*DefaultDeployRetryDelay)
	}

	// Disabled retries still allow one attempt; negative inputs never
	// produce a non-positive budget.
	if b := deployPhaseBudget(-1, time.Hour); b != deployAttemptBudget {
		t.Errorf("attempts=-1 budget = %s, want %s", b, deployAttemptBudget)
	}
	if b := deployPhaseBudget(2, -time.Second); b <= 0 {
		t.Errorf("negative delay budget = %s, want positive", b)
	}
}

// buildRetryHandler wires a Receiver around an arbitrary Deployer (no real
// stack engine) with an injected 1ms retry delay, so the HTTP-level retry
// tests exercise the full signed-request path without sleeping 30s.
func buildRetryHandler(t *testing.T, sourceName string, dep Deployer) (*httptest.Server, *store.Store, *credentials.Cipher, []byte) {
	t.Helper()
	st, c, _, secret := testDeps(t, sourceName)
	h := &Receiver{
		Sources: NewLocal(st, c),
		Deploy:  dep,
		Record:  NewLocal(st, c),
		// Production policy (2 retries) with an injected delay so the test
		// does not wait the DefaultDeployRetryDelay between attempts.
		RetryDelay: time.Millisecond,
	}
	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, st, c, secret
}

// postSigned sends a signed webhook POST and returns status + body.
func postSigned(t *testing.T, srv *httptest.Server, source string, secret, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+source, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	now := time.Now().Unix()
	req.Header.Set("X-Pmcluster-Timestamp", strconv.FormatInt(now, 10))
	req.Header.Set("X-Pmcluster-Signature", computeHMAC(secret, body, now))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestReceiverRetriesThenRecordsAccepted: the deploy fails twice and
// succeeds on the third attempt — the caller sees 200 and history holds
// exactly ONE delivery row (one request = one row) with retries=2 and the
// accepted status.
func TestReceiverRetriesThenRecordsAccepted(t *testing.T) {
	const sourceName = "github-retry-ok"

	dep := &scriptedDeployer{
		failures: 2,
		err:      errors.New("docker stack deploy failed"),
		result:   stacks.Result{StackName: "whoami-webhook", Revision: 7},
	}
	srv, st, c, secret := buildRetryHandler(t, sourceName, dep)

	code, body := postSigned(t, srv, sourceName, secret, mustJSONPayload(t))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", code, body)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if parsed["retries"] != float64(2) {
		t.Errorf("response retries = %v, want 2", parsed["retries"])
	}
	if got := dep.callCount(); got != 3 {
		t.Errorf("deploy calls = %d, want 3", got)
	}

	ds, err := NewLocal(st, c).Deliveries(context.Background(), sourceName, 0)
	if err != nil {
		t.Fatalf("Deliveries: %v", err)
	}
	if len(ds) != 1 {
		t.Fatalf("delivery rows = %d, want 1 (retries must not add rows)", len(ds))
	}
	d := ds[0]
	if d.Status != "accepted" {
		t.Errorf("status = %q, want accepted", d.Status)
	}
	if d.Retries != 2 {
		t.Errorf("retries = %d, want 2", d.Retries)
	}
	if d.StackName != "whoami-webhook" || d.Revision != 7 {
		t.Errorf("delivery = %+v, want stack whoami-webhook rev 7", d)
	}
	if d.Error != "" {
		t.Errorf("error = %q, want empty on success", d.Error)
	}
}

// TestReceiverRetriesExhaustedRecordsServerError: every attempt fails — the
// caller sees 502 plus the retry count, and history records one
// server_error row with retries=2 and the final error message.
func TestReceiverRetriesExhaustedRecordsServerError(t *testing.T) {
	const sourceName = "github-retry-fail"

	dep := &scriptedDeployer{
		failures: -1,
		err:      errors.New("docker stack deploy failed: swarm node busy"),
	}
	srv, st, c, secret := buildRetryHandler(t, sourceName, dep)

	code, body := postSigned(t, srv, sourceName, secret, mustJSONPayload(t))
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", code, body)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if parsed["retries"] != float64(2) {
		t.Errorf("response retries = %v, want 2", parsed["retries"])
	}
	if got := dep.callCount(); got != 3 {
		t.Errorf("deploy calls = %d, want 3 (1 + 2 retries)", got)
	}

	ds, err := NewLocal(st, c).Deliveries(context.Background(), sourceName, 0)
	if err != nil {
		t.Fatalf("Deliveries: %v", err)
	}
	if len(ds) != 1 {
		t.Fatalf("delivery rows = %d, want 1", len(ds))
	}
	d := ds[0]
	if d.Status != "server_error" {
		t.Errorf("status = %q, want server_error", d.Status)
	}
	if d.Retries != 2 {
		t.Errorf("retries = %d, want 2 (budget exhausted)", d.Retries)
	}
	if d.Error != "docker stack deploy failed: swarm node busy" {
		t.Errorf("error = %q, want the final error message", d.Error)
	}
}

// TestDeliveriesJSONIncludesRetries pins the HTTP shape: the deliveries
// endpoint carries the retry count so the console/CLI can render it.
func TestDeliveriesJSONIncludesRetries(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLocal(t)

	if err := svc.Record(ctx, &Delivery{
		Source:  "github-prod",
		Status:  "server_error",
		Retries: 2,
		Error:   "docker stack deploy failed",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	h := &HTTP{Svc: svc}
	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/webhooks/github-prod/deliveries")
	if err != nil {
		t.Fatalf("GET deliveries: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Deliveries []struct {
			Status  string `json:"status"`
			Retries int    `json:"retries"`
			Error   string `json:"error"`
		} `json:"deliveries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Deliveries) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(out.Deliveries))
	}
	got := out.Deliveries[0]
	if got.Retries != 2 || got.Status != "server_error" || got.Error == "" {
		t.Errorf("delivery JSON = %+v, want retries 2 with server_error and its error", got)
	}
}

// mustJSONPayload marshals the canonical valid webhook payload.
func mustJSONPayload(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(retryTestPayload())
	if err != nil {
		t.Fatalf("json.Marshal payload: %v", err)
	}
	return b
}
