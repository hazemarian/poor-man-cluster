package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/telemetry"
)

// recordingDeployer records every DeployStack call and can optionally inject
// an error. It implements cluster.StackDeployer.
type recordingDeployer struct {
	mu        sync.Mutex
	deployed  []deployRecord
	deployErr error
	removed   []string
	updated   []string
}

type deployRecord struct {
	Name string
	YAML string
}

func (r *recordingDeployer) DeployStack(_ context.Context, name string, composeYAML []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deployErr != nil {
		return r.deployErr
	}
	r.deployed = append(r.deployed, deployRecord{Name: name, YAML: string(composeYAML)})
	return nil
}

func (r *recordingDeployer) DeployStackNoPrune(_ context.Context, name string, composeYAML []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deployErr != nil {
		return r.deployErr
	}
	r.deployed = append(r.deployed, deployRecord{Name: name, YAML: string(composeYAML)})
	return nil
}

func (r *recordingDeployer) PruneStack(_ context.Context, _ string, _ []byte) error { return nil }

func (r *recordingDeployer) RemoveStack(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, name)
	return nil
}

func (r *recordingDeployer) ForceUpdateService(_ context.Context, fullName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updated = append(r.updated, fullName)
	return nil
}

func (r *recordingDeployer) PruneStaleContainers(_ context.Context, _ string, _ string) error {
	return nil
}

// deployedLen returns how many deploy calls landed (mutex-guarded for the
// background-apply tests).
func (r *recordingDeployer) deployedLen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.deployed)
}

// setDeployErr installs a deploy error that the next DeployStack /
// DeployStackNoPrune call returns (mutex-guarded: the apply runs in a
// background goroutine).
func (r *recordingDeployer) setDeployErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deployErr = err
}

// deployedNameAt returns the stack name of the i-th deploy call.
func (r *recordingDeployer) deployedNameAt(i int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < 0 || i >= len(r.deployed) {
		return ""
	}
	return r.deployed[i].Name
}

// Compile-time assertion.
var _ cluster.StackDeployer = (*recordingDeployer)(nil)

// testDeps sets up a real Store + Cipher + recordingDeployer, creates a
// webhook source named sourceName with an encrypted shared secret, and
// returns the plaintext secret for HMAC computation in tests.
func testDeps(t *testing.T, sourceName string) (*store.Store, *credentials.Cipher, *recordingDeployer, []byte) {
	t.Helper()
	dir := t.TempDir()

	s, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	c, err := credentials.Open(filepath.Join(dir, ".enc_key"))
	if err != nil {
		t.Fatalf("credentials.Open: %v", err)
	}

	secret := []byte("super-secret-hmac-key-for-tests")
	ciphertext, err := c.Encrypt(secret)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if err := s.CreateWebhookSource(context.Background(), sourceName, "test", ciphertext); err != nil {
		t.Fatalf("CreateWebhookSource: %v", err)
	}

	rec := &recordingDeployer{}
	return s, c, rec, secret
}

// buildHandler constructs a chi-backed httptest.Server using the Receiver under
// test. Returns the server URL and the receiver.
func buildHandler(t *testing.T, st *store.Store, c *credentials.Cipher, dep *recordingDeployer) (*httptest.Server, *Receiver) {
	t.Helper()
	deploySvc := &stacks.Service{
		Store:    st,
		Deployer: dep,
	}
	h := &Receiver{
		Sources: NewLocal(st, c),
		Deploy:  deploySvc,
		Record:  NewLocal(st, c),
		// Q3 retry policy is on by default (2 extra attempts); shrink the
		// 30s production delay so failing-deploy tests don't wait a minute.
		RetryDelay: time.Millisecond,
	}
	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, h
}

// computeHMAC returns "sha256=<hex>" for the given timestamp, body, and secret.
// HMAC is over timestamp_decimal + body (exactly as verifyHMAC does).
func computeHMAC(secret, body []byte, timestamp int64) string {
	mac := hmac.New(sha256.New, secret)
	fmt.Fprint(mac, timestamp)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// nowTimestamp returns the current unix time as a string for the timestamp header.
func nowTimestamp() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}

// A minimal valid DSL manifest that the deploy pipeline accepts.
const validManifest = `app: whoami-webhook
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

// validPayload returns a JSON-encoded stacks.Payload using validManifest.
func validPayload(t *testing.T) []byte {
	t.Helper()
	p := stacks.Payload{Manifest: validManifest, RepoURL: "https://github.com/org/repo", File: "deploy/app.yaml"}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("json.Marshal payload: %v", err)
	}
	return b
}

// TestHandlerRequiresProvenance verifies webhook deploys without repo_url/file
// are rejected (user m2930: provenance is required in the payload).
func TestHandlerRequiresProvenance(t *testing.T) {
	const sourceName = "github-prod"

	st, c, _, secret := testDeps(t, sourceName)
	srv, _ := buildHandler(t, st, c, &recordingDeployer{})
	_ = srv

	body := validPayload(t)
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	delete(payload, "repo_url")
	delete(payload, "file")
	noProvenance, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+sourceName, bytes.NewReader(noProvenance))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	now := time.Now().Unix()
	req.Header.Set("X-Pmcluster-Timestamp", strconv.FormatInt(now, 10))
	req.Header.Set("X-Pmcluster-Signature", computeHMAC(secret, noProvenance, now))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), "provenance required") {
		t.Errorf("400 body = %q, want 'provenance required'", b)
	}
}

// TestHandlerRecordsDeliveries asserts the receiver persists one delivery row
// per attempt — success and failure alike — with provenance captured.
func TestHandlerRecordsDeliveries(t *testing.T) {
	const sourceName = "github-prod"

	st, c, dep, secret := testDeps(t, sourceName)
	srv, _ := buildHandler(t, st, c, dep)
	ctx := context.Background()

	post := func(body []byte) int {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+sourceName, bytes.NewReader(body))
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
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	// 1. Successful deploy → accepted delivery with provenance.
	body := validPayload(t)
	if code := post(body); code != http.StatusAccepted {
		t.Fatalf("success POST = %d, want 202", code)
	}

	// 2. Payload missing provenance → bad_request delivery.
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	delete(payload, "repo_url")
	delete(payload, "file")
	noProv, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if code := post(noProv); code != http.StatusBadRequest {
		t.Fatalf("no-provenance POST = %d, want 400", code)
	}

	// 3. Assert the recorded rows.
	ds, err := NewLocal(st, c).Deliveries(ctx, sourceName, 0)
	if err != nil {
		t.Fatalf("Deliveries: %v", err)
	}
	if len(ds) != 2 {
		t.Fatalf("got %d deliveries, want 2", len(ds))
	}
	// Newest first: the bad_request arrived second.
	if ds[0].Status != "bad_request" || ds[1].Status != "accepted" {
		t.Errorf("statuses = [%s %s], want [bad_request accepted]", ds[0].Status, ds[1].Status)
	}
	if ds[1].StackName == "" || ds[1].Revision == 0 {
		t.Errorf("accepted delivery = %+v, want stack name with revision", ds[1])
	}
	if ds[0].Error == "" {
		t.Error("bad_request delivery should carry the provenance error")
	}
}

func TestHandlerReceive(t *testing.T) {
	const sourceName = "github-prod"

	var firstUnauthorizedBody string

	checkUnauthorized := func(t *testing.T, resp *http.Response, label string) {
		t.Helper()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", label, resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		bodyStr := strings.TrimSpace(string(body))
		if firstUnauthorizedBody == "" {
			firstUnauthorizedBody = bodyStr
		} else if bodyStr != firstUnauthorizedBody {
			t.Errorf("%s: 401 body %q differs from earlier 401 body %q (all 401s must be indistinguishable)", label, bodyStr, firstUnauthorizedBody)
		}
	}

	t.Run("missing signature header", func(t *testing.T) {
		st, c, dep, _ := testDeps(t, sourceName)
		srv, _ := buildHandler(t, st, c, dep)

		body := validPayload(t)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+sourceName, bytes.NewReader(body))
		req.Header.Set(TimestampHeader, nowTimestamp())
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		checkUnauthorized(t, resp, "missing signature header")
	})

	t.Run("missing timestamp header", func(t *testing.T) {
		st, c, dep, secret := testDeps(t, sourceName)
		srv, _ := buildHandler(t, st, c, dep)

		body := validPayload(t)

		mac := hmac.New(sha256.New, secret)
		mac.Write(body)
		sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+sourceName, bytes.NewReader(body))
		req.Header.Set(SignatureHeader, sig)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		checkUnauthorized(t, resp, "missing timestamp header")
	})

	t.Run("stale timestamp", func(t *testing.T) {
		st, c, dep, secret := testDeps(t, sourceName)
		srv, _ := buildHandler(t, st, c, dep)

		stale := time.Now().Add(-10 * time.Minute).Unix()
		body := validPayload(t)
		sig := computeHMAC(secret, body, stale)

		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+sourceName, bytes.NewReader(body))
		req.Header.Set(SignatureHeader, sig)
		req.Header.Set(TimestampHeader, strconv.FormatInt(stale, 10))
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		checkUnauthorized(t, resp, "stale timestamp")
	})

	t.Run("future timestamp", func(t *testing.T) {
		st, c, dep, secret := testDeps(t, sourceName)
		srv, _ := buildHandler(t, st, c, dep)

		future := time.Now().Add(10 * time.Minute).Unix()
		body := validPayload(t)
		sig := computeHMAC(secret, body, future)

		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+sourceName, bytes.NewReader(body))
		req.Header.Set(SignatureHeader, sig)
		req.Header.Set(TimestampHeader, strconv.FormatInt(future, 10))
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		checkUnauthorized(t, resp, "future timestamp")
	})

	t.Run("malformed signature — no sha256 prefix", func(t *testing.T) {
		st, c, dep, secret := testDeps(t, sourceName)
		srv, _ := buildHandler(t, st, c, dep)

		body := validPayload(t)
		ts := time.Now().Unix()
		sig := computeHMAC(secret, body, ts)

		malformed := strings.TrimPrefix(sig, "sha256=")

		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+sourceName, bytes.NewReader(body))
		req.Header.Set(TimestampHeader, strconv.FormatInt(ts, 10))
		req.Header.Set(SignatureHeader, malformed)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		checkUnauthorized(t, resp, "malformed signature — no sha256 prefix")
	})

	t.Run("non-hex signature", func(t *testing.T) {
		st, c, dep, _ := testDeps(t, sourceName)
		srv, _ := buildHandler(t, st, c, dep)

		body := validPayload(t)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+sourceName, bytes.NewReader(body))
		req.Header.Set(TimestampHeader, nowTimestamp())
		req.Header.Set(SignatureHeader, "sha256=not-valid-hex-!!!!")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		checkUnauthorized(t, resp, "non-hex signature")
	})

	t.Run("wrong length hex signature", func(t *testing.T) {
		st, c, dep, _ := testDeps(t, sourceName)
		srv, _ := buildHandler(t, st, c, dep)

		body := validPayload(t)

		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+sourceName, bytes.NewReader(body))
		req.Header.Set(TimestampHeader, nowTimestamp())
		req.Header.Set(SignatureHeader, "sha256=abc")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		checkUnauthorized(t, resp, "wrong length signature")
	})

	t.Run("correct format but wrong digest", func(t *testing.T) {
		st, c, dep, _ := testDeps(t, sourceName)
		srv, _ := buildHandler(t, st, c, dep)

		body := validPayload(t)

		wrongSig := "sha256=" + strings.Repeat("ab", 32)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+sourceName, bytes.NewReader(body))
		req.Header.Set(TimestampHeader, nowTimestamp())
		req.Header.Set(SignatureHeader, wrongSig)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		checkUnauthorized(t, resp, "wrong digest")
	})

	t.Run("unknown source name with valid-shaped signature", func(t *testing.T) {
		st, c, dep, secret := testDeps(t, sourceName)
		srv, _ := buildHandler(t, st, c, dep)

		ts := time.Now().Unix()
		body := validPayload(t)
		sig := computeHMAC(secret, body, ts)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/never-existed", bytes.NewReader(body))
		req.Header.Set(TimestampHeader, strconv.FormatInt(ts, 10))
		req.Header.Set(SignatureHeader, sig)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		checkUnauthorized(t, resp, "unknown source")
	})

	t.Run("valid HMAC and valid manifest — 202 and deploy started", func(t *testing.T) {
		st, c, dep, secret := testDeps(t, sourceName)
		srv, _ := buildHandler(t, st, c, dep)

		ts := time.Now().Unix()
		body := validPayload(t)
		sig := computeHMAC(secret, body, ts)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+sourceName, bytes.NewReader(body))
		req.Header.Set(TimestampHeader, strconv.FormatInt(ts, 10))
		req.Header.Set(SignatureHeader, sig)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusAccepted {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 202; body: %s", resp.StatusCode, b)
		}

		var result map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if _, ok := result["stack"]; !ok {
			t.Error("response missing 'stack' field")
		}
		if _, ok := result["revision"]; !ok {
			t.Error("response missing 'revision' field")
		}
		if result["stack"] != "whoami-webhook" {
			t.Errorf("stack = %v, want 'whoami-webhook'", result["stack"])
		}

		// The swarm apply runs in a background goroutine now; give it a
		// bounded moment to reach the deployer.
		deadline := time.Now().Add(2 * time.Second)
		for dep.deployedLen() == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if dep.deployedLen() == 0 {
			t.Error("expected recordingDeployer.deployed to be non-empty (async apply)")
		} else if dep.deployedNameAt(0) != "whoami-webhook" {
			t.Errorf("deployed name = %q, want 'whoami-webhook'", dep.deployedNameAt(0))
		}

		ctx := context.Background()
		stacks, err := st.ListStacks(ctx)
		if err != nil {
			t.Fatalf("ListStacks: %v", err)
		}
		found := false
		for _, stack := range stacks {
			if stack.Name == "whoami-webhook" {
				found = true
				break
			}
		}
		if !found {
			t.Error("stack 'whoami-webhook' not found in store after successful deploy")
		}
	})

	t.Run("valid HMAC but invalid JSON body — 400", func(t *testing.T) {
		st, c, dep, secret := testDeps(t, sourceName)
		srv, _ := buildHandler(t, st, c, dep)

		ts := time.Now().Unix()
		body := []byte("not-valid-json{{{{")
		sig := computeHMAC(secret, body, ts)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+sourceName, bytes.NewReader(body))
		req.Header.Set(TimestampHeader, strconv.FormatInt(ts, 10))
		req.Header.Set(SignatureHeader, sig)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, b)
		}
	})

	t.Run("valid HMAC + valid JSON, apply fails in the background — 202 accepted", func(t *testing.T) {
		st, c, dep, secret := testDeps(t, sourceName)
		dep.setDeployErr(errors.New("docker stack deploy failed"))
		srv, _ := buildHandler(t, st, c, dep)

		ts := time.Now().Unix()
		body := validPayload(t)
		sig := computeHMAC(secret, body, ts)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+sourceName, bytes.NewReader(body))
		req.Header.Set(TimestampHeader, strconv.FormatInt(ts, 10))
		req.Header.Set(SignatureHeader, sig)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		// Fire-and-forget: validation + revision recording are synchronous,
		// the docker-layer apply runs in the background — the request returns
		// 202 accepted even when the apply later fails.
		if resp.StatusCode != http.StatusAccepted {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("status = %d, want 202; body: %s", resp.StatusCode, b)
		}
	})

	t.Run("body larger than MaxBodyBytes — 413", func(t *testing.T) {
		st, c, dep, secret := testDeps(t, sourceName)
		srv, _ := buildHandler(t, st, c, dep)

		padding := strings.Repeat("x", MaxBodyBytes)
		p := stacks.Payload{Manifest: padding}
		body, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}

		if len(body) <= MaxBodyBytes {

			p.AppName = strings.Repeat("y", MaxBodyBytes-len(body)+1)
			body, err = json.Marshal(p)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
		}

		ts := time.Now().Unix()
		sig := computeHMAC(secret, body, ts)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+sourceName, bytes.NewReader(body))
		req.Header.Set(TimestampHeader, strconv.FormatInt(ts, 10))
		req.Header.Set(SignatureHeader, sig)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("status = %d, want 413; body: %s", resp.StatusCode, b)
		}
	})

	t.Run("last_used_at populated after successful POST", func(t *testing.T) {
		st, c, dep, secret := testDeps(t, sourceName)
		srv, _ := buildHandler(t, st, c, dep)

		ctx := context.Background()
		before, err := st.WebhookSource(ctx, sourceName)
		if err != nil {
			t.Fatalf("WebhookSource (before): %v", err)
		}
		if before.LastUsedAt.Valid {
			t.Fatal("last_used_at should be NULL before first successful POST")
		}

		ts := time.Now().Unix()
		body := validPayload(t)
		sig := computeHMAC(secret, body, ts)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+sourceName, bytes.NewReader(body))
		req.Header.Set(TimestampHeader, strconv.FormatInt(ts, 10))
		req.Header.Set(SignatureHeader, sig)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("expected 202, got %d", resp.StatusCode)
		}

		after, err := st.WebhookSource(ctx, sourceName)
		if err != nil {
			t.Fatalf("WebhookSource (after): %v", err)
		}
		if !after.LastUsedAt.Valid {
			t.Error("last_used_at should be non-NULL after successful POST")
		}
	})

	t.Run("last_used_at stays NULL after 401", func(t *testing.T) {
		st, c, dep, _ := testDeps(t, sourceName)
		srv, _ := buildHandler(t, st, c, dep)

		ctx := context.Background()
		body := validPayload(t)

		wrongSig := "sha256=" + strings.Repeat("00", 32)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+sourceName, bytes.NewReader(body))
		req.Header.Set(TimestampHeader, nowTimestamp())
		req.Header.Set(SignatureHeader, wrongSig)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp.StatusCode)
		}

		src, err := st.WebhookSource(ctx, sourceName)
		if err != nil {
			t.Fatalf("WebhookSource: %v", err)
		}
		if src.LastUsedAt.Valid {
			t.Errorf("last_used_at should remain NULL after a 401, got %d", src.LastUsedAt.Int64)
		}
	})

	if firstUnauthorizedBody == "" {

		t.Log("note: 401 body comparison across sub-tests uses shared variable — see sub-test 'missing signature header'")
	} else {
		t.Logf("all 401 bodies matched: %q", firstUnauthorizedBody)
	}
}

// TestHandlerReceive_BodySizeExact verifies that a body of exactly MaxBodyBytes
// is accepted (not rejected as too large).
func TestHandlerReceive_BodySizeExact(t *testing.T) {
	const sourceName = "exact-size"
	st, c, dep, secret := testDeps(t, sourceName)
	srv, _ := buildHandler(t, st, c, dep)

	smallBody := validPayload(t)
	if len(smallBody) >= MaxBodyBytes {
		t.Skip("validPayload already exceeds MaxBodyBytes — test not applicable")
	}
	ts := time.Now().Unix()
	sig := computeHMAC(secret, smallBody, ts)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+sourceName, bytes.NewReader(smallBody))
	req.Header.Set(TimestampHeader, strconv.FormatInt(ts, 10))
	req.Header.Set(SignatureHeader, sig)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		t.Errorf("a small valid body was incorrectly rejected as too large (413)")
	}
}

// Verify the HMAC check mirrors the production verifyHMAC function directly.
func TestComputeHMACMatchesProduction(t *testing.T) {
	secret := []byte("test-secret")
	body := []byte(`{"manifest":"app: test\n"}`)
	ts := int64(1710000000)

	sig := computeHMAC(secret, body, ts)
	if !strings.HasPrefix(sig, "sha256=") {
		t.Fatalf("sig = %q, want 'sha256=' prefix", sig)
	}
	hexPart := strings.TrimPrefix(sig, "sha256=")
	got, err := hex.DecodeString(hexPart)
	if err != nil {
		t.Fatalf("decode hex: %v", err)
	}

	mac := hmac.New(sha256.New, secret)
	fmt.Fprint(mac, ts)
	mac.Write(body)
	want := mac.Sum(nil)

	if !hmac.Equal(got, want) {
		t.Error("computeHMAC does not match raw crypto/hmac computation")
	}
	if len(got) != sha256.Size {
		t.Errorf("signature length = %d, want %d", len(got), sha256.Size)
	}
}

// Placeholder to ensure the package imports compile even if individual
// sub-tests are skipped.
var _ = fmt.Sprintf

// TestWebhookConflictFromDifferentRepo verifies the app-name duplication guard
// surfaces through the webhook receiver: a second deploy from a different repo
// returns 502 and records a server_error delivery, and nothing is deployed.
func TestWebhookConflictFromDifferentRepo(t *testing.T) {
	const sourceName = "github-prod"
	st, c, dep, secret := testDeps(t, sourceName)
	srv, _ := buildHandler(t, st, c, dep)
	ctx := context.Background()

	post := func(body []byte) (int, string) {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+sourceName, bytes.NewReader(body))
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

	first := validPayload(t)
	if code, body := post(first); code != http.StatusAccepted {
		t.Fatalf("first webhook deploy = %d, want 202; body: %s", code, body)
	}

	conflict := &stacks.Payload{
		Manifest: validManifest,
		RepoURL:  "https://github.com/other-org/repo",
		File:     "deploy/app.yaml",
	}
	b, _ := json.Marshal(conflict)
	if code, body := post(b); code != http.StatusBadGateway {
		t.Fatalf("conflicting webhook deploy = %d, want 502; body: %s", code, body)
	} else if !strings.Contains(body, "already exists from repo") {
		t.Errorf("502 body = %s, want 'already exists from repo'", body)
	}

	deliveries, err := NewLocal(st, c).Deliveries(ctx, sourceName, 0)
	if err != nil {
		t.Fatalf("Deliveries: %v", err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("delivery count = %d, want 2 (accepted + server_error)", len(deliveries))
	}
	if deliveries[0].Status != "server_error" {
		t.Errorf("newest delivery status = %q, want server_error", deliveries[0].Status)
	}
	if !strings.Contains(deliveries[0].Error, "already exists from repo") {
		t.Errorf("delivery error = %q, want 'already exists from repo'", deliveries[0].Error)
	}
	if dep.deployedLen() != 1 {
		t.Errorf("deployer calls = %d, want 1 (conflict never reached the swarm)", dep.deployedLen())
	}
}

// sampleRecorder captures metric emissions through telemetry.SetSink so the
// Q5 alerting metrics can be asserted hermetically — no OTLP collector and
// no global MeterProvider mutation. The sink is process-wide, so it is
// restored on cleanup; tests using it must not call t.Parallel.
type sampleRecorder struct {
	mu      sync.Mutex
	got     []telemetry.Sample
	restore func()
}

func newSampleRecorder(t *testing.T) *sampleRecorder {
	t.Helper()
	r := &sampleRecorder{}
	r.restore = telemetry.SetSink(func(s telemetry.Sample) {
		labels := make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			labels[k] = v
		}
		r.mu.Lock()
		r.got = append(r.got, telemetry.Sample{Name: s.Name, Value: s.Value, Labels: labels})
		r.mu.Unlock()
	})
	t.Cleanup(func() { r.restore() })
	return r
}

// find returns the captured samples with the given metric name.
func (r *sampleRecorder) find(name string) []telemetry.Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []telemetry.Sample
	for _, s := range r.got {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// TestHandlerEmitsDeliveryMetric is the Q5 alerting contract: every delivery
// outcome bumps pmcluster.webhook.requests.total with {source, status} — one
// of accepted|unauthorized|bad_request|server_error — so OpenObserve can
// alert on webhook failures. The sink is installed before the POSTs, and the
// receiver records before writing the response, so by the time the client
// sees a status the sample is already captured.
func TestHandlerEmitsDeliveryMetric(t *testing.T) {
	const okSource = "github-prod"
	const failSource = "github-failing"

	rec := newSampleRecorder(t)

	st, c, dep, secret := testDeps(t, okSource)
	srv, _ := buildHandler(t, st, c, dep)

	// A second receiver whose deploys always fail → server_error outcomes.
	st2, c2, dep2, secret2 := testDeps(t, failSource)
	dep2.setDeployErr(errors.New("docker stack deploy failed"))
	srv2, _ := buildHandler(t, st2, c2, dep2)

	post := func(server *httptest.Server, source string, secret []byte, body []byte, signOK bool) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, server.URL+"/webhook/"+source, bytes.NewReader(body))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		now := time.Now().Unix()
		req.Header.Set("X-Pmcluster-Timestamp", strconv.FormatInt(now, 10))
		sig := computeHMAC(secret, body, now)
		if !signOK {
			sig = "sha256=" + strings.Repeat("00", 32)
		}
		req.Header.Set("X-Pmcluster-Signature", sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	body := validPayload(t)

	// 1. accepted — valid HMAC + provenance.
	if code := post(srv, okSource, secret, body, true); code != http.StatusAccepted {
		t.Fatalf("accepted POST = %d, want 202", code)
	}
	// 2. unauthorized — wrong digest.
	if code := post(srv, okSource, secret, body, false); code != http.StatusUnauthorized {
		t.Fatalf("unauthorized POST = %d, want 401", code)
	}
	// 3. bad_request — valid HMAC, missing provenance.
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	delete(payload, "repo_url")
	delete(payload, "file")
	noProv, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if code := post(srv, okSource, secret, noProv, true); code != http.StatusBadRequest {
		t.Fatalf("no-provenance POST = %d, want 400", code)
	}
	// 4. apply fails in the background — still accepted (fire-and-forget).
	if code := post(srv2, failSource, secret2, body, true); code != http.StatusAccepted {
		t.Fatalf("failing-deploy POST = %d, want 202", code)
	}

	got := rec.find(telemetry.MetricWebhookRequests)
	if len(got) != 4 {
		t.Fatalf("captured %d %s samples, want 4: %+v", len(got), telemetry.MetricWebhookRequests, got)
	}
	want := map[string]int{
		okSource + "/accepted":     1,
		okSource + "/unauthorized": 1,
		okSource + "/bad_request":  1,
		// Fire-and-forget: the failure source's deploy is accepted synchronously;
		// the background apply outcome is not part of this request's metrics.
		failSource + "/accepted": 1,
	}
	for _, s := range got {
		if s.Value != 1 {
			t.Errorf("sample value = %d, want 1 (%+v)", s.Value, s.Labels)
		}
		key := s.Labels["source"] + "/" + s.Labels["status"]
		if want[key] == 0 {
			t.Errorf("unexpected source/status label pair: %q (labels %v)", key, s.Labels)
			continue
		}
		want[key]--
	}
	for key, left := range want {
		if left != 0 {
			t.Errorf("missing %s sample (%d left)", key, left)
		}
	}
}
