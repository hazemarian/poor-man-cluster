package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// slowDeployer answers only after the request has almost certainly given up: the
// shape of a deploy phase that outlives the caller.
type slowDeployer struct{ delay time.Duration }

func (s slowDeployer) Deploy(ctx context.Context, p stacks.Payload) (*stacks.Result, error) {
	return nil, context.DeadlineExceeded
}

func (s slowDeployer) DeployAsync(_ context.Context, p stacks.Payload) (*stacks.Result, error) {
	time.Sleep(s.delay)
	return &stacks.Result{StackName: "slow-stack", Revision: 42}, nil
}

// A deploy phase longer than the response budget must answer 202 with an honest
// "still running" body, not hold the request open until the ingress proxy cuts it.
//
// This is the second half of the production bug: a receiver that blocks for the
// whole retry window cannot be reached through a 30s proxy at all.
func TestResponseBudgetAnswersBeforeTheIngressCutsTheRequest(t *testing.T) {
	old := webhookResponseBudget
	webhookResponseBudget = 20 * time.Millisecond
	t.Cleanup(func() { webhookResponseBudget = old })

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c, err := credentials.Open(filepath.Join(dir, ".enc_key"))
	if err != nil {
		t.Fatalf("credentials.Open: %v", err)
	}

	const source = "github-slow"
	plain, err := NewLocal(st, c).Create(context.Background(), source, "test")
	if err != nil {
		t.Fatalf("Create source: %v", err)
	}
	secret := []byte(plain)

	rec := NewLocal(st, c)
	h := &Receiver{
		Sources:    NewLocal(st, c),
		Deploy:     slowDeployer{delay: 300 * time.Millisecond},
		Record:     rec,
		RetryDelay: time.Millisecond,
	}
	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(&stacks.Payload{
		AppName:  "slow-stack",
		Manifest: validManifest,
		RepoURL:  "https://github.com/org/slow",
		File:     "deploy/slow.yaml",
	})
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/webhook/"+source, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	now := time.Now().Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pmcluster-Timestamp", strconv.FormatInt(now, 10))
	req.Header.Set("X-Pmcluster-Signature", computeHMAC(secret, body, now))

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", resp.StatusCode, payload)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("response took %s — it waited for the deploy phase instead of the budget", elapsed)
	}
	if !strings.Contains(string(payload), `"pending":true`) {
		t.Fatalf("body = %s, want pending:true", payload)
	}

	// The delivery row is still written once the phase finishes: the response and
	// the history are different records, and only one of them is time-boxed.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ds, err := rec.Deliveries(context.Background(), source, 0)
		if err == nil && len(ds) == 1 && ds[0].Status == "accepted" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	ds, _ := rec.Deliveries(context.Background(), source, 0)
	t.Fatalf("delivery history not recorded after the phase finished: %+v", ds)
}

// The budget must stay under the edge proxy's REQUEST_TIMEOUT (30s, see
// internal/edgeproxy/config.go), or the proxy answers first and the caller never
// sees anything this receiver wrote.
func TestResponseBudgetStaysUnderTheIngressCeiling(t *testing.T) {
	const edgeProxyRequestTimeout = 30 * time.Second
	if webhookResponseBudget >= edgeProxyRequestTimeout {
		t.Fatalf("webhookResponseBudget = %s, must stay under the edge proxy's %s", webhookResponseBudget, edgeProxyRequestTimeout)
	}
}
