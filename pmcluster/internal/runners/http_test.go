package runners

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// storeRow is the row a daemon has after a reconcile pass: the local cache a kill
// falls back to when GitHub cannot be reached.
func storeRow(name, org, node string) store.GitHubRunner {
	return store.GitHubRunner{Name: name, Org: org, Node: node, Mode: ModeEphemeral, State: StateIdle, LastSeen: 1}
}

// The console talks to these three routes, so they are tested as HTTP: status
// codes and bodies are the contract the panel and curl share.

func newRunnerHTTP(t *testing.T, h *harness) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	(&HTTP{Mgr: h.mgr}).Mount(r)
	return r
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestKillUnknownRunnerIs404(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1"})
	rec := do(t, newRunnerHTTP(t, h), http.MethodPost, "/github/runners/pmc-nope-org/kill", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (unknown runner), body %s", rec.Code, rec.Body.String())
	}
}

func TestBusyKillNeedsConfirm(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1"})
	h.docker.containers["pmc-worker-1-org"] = Container{ID: "c1", Name: "pmc-worker-1-org", State: "running"}
	id := h.github.addRunner("acme", "pmc-worker-1-org", "online")
	h.github.runners["acme"][0].Busy = true // the runner is executing a job
	srv := newRunnerHTTP(t, h)

	rec := do(t, srv, http.MethodPost, "/github/runners/pmc-worker-1-org/kill", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 without confirm (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "confirm") {
		t.Fatalf("body = %s, want it to name the confirm flag", rec.Body.String())
	}
	if len(h.docker.removed) != 0 {
		t.Fatal("a busy runner was killed without confirmation")
	}

	rec = do(t, srv, http.MethodPost, "/github/runners/pmc-worker-1-org/kill", `{"confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d with confirm, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out KillOutcome
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.ContainerRemoved || !out.Deregistered || !out.Held {
		t.Fatalf("outcome = %+v, want container removed, deregistered and held", out)
	}
	if len(h.docker.removed) != 1 {
		t.Fatalf("removed = %v, want the container", h.docker.removed)
	}
	if len(h.github.deleted) != 1 || h.github.deleted[0] != "acme#"+itoa(id) {
		t.Fatalf("deleted = %v, want one de-registration", h.github.deleted)
	}
	// The hold keeps the next pass from re-registering it.
	if res, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	} else if len(res.Registered) != 0 {
		t.Fatalf("the reconcile re-registered a killed runner: %v", res.Registered)
	}
}

// A GitHub outage must not turn a kill into a 500: the container is gone, and the
// body has to name the half that did not happen.
func TestKillWithGitHubUnreachableStillRemovesTheContainer(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1"})
	h.docker.containers["pmc-worker-1-org"] = Container{ID: "c1", Name: "pmc-worker-1-org", State: "running"}
	h.github.addRunner("acme", "pmc-worker-1-org", "online")
	srv := newRunnerHTTP(t, h)

	// findRunner falls back to the local row once GitHub is unreachable, so seed
	// one: this is what a real daemon has after a reconcile pass.
	if err := h.store.PutGitHubRunner(h.ctx, storeRow("pmc-worker-1-org", "acme", "worker-1")); err != nil {
		t.Fatal(err)
	}
	h.github.listErr = errors.New("dial tcp 140.82.121.6:443: i/o timeout")

	rec := do(t, srv, http.MethodPost, "/github/runners/pmc-worker-1-org/kill", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with the failure in the body (body %s)", rec.Code, rec.Body.String())
	}
	var out KillOutcome
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.ContainerRemoved {
		t.Fatalf("outcome = %+v, want the container removed", out)
	}
	if out.Deregistered {
		t.Fatalf("outcome claims a de-registration GitHub never confirmed: %+v", out)
	}
	if out.DeregisterError == "" {
		t.Fatalf("outcome = %+v, want deregister_error naming the failure", out)
	}
	if len(h.docker.removed) != 1 {
		t.Fatalf("removed = %v, want the container", h.docker.removed)
	}
}

func TestRestartRegistersTheRunnerAgain(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1"})
	if _, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	srv := newRunnerHTTP(t, h)
	if rec := do(t, srv, http.MethodPost, "/github/runners/pmc-worker-1-org/kill", ""); rec.Code != http.StatusOK {
		t.Fatalf("kill status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if len(h.docker.containers) != 0 {
		t.Fatalf("the kill left a container: %v", h.docker.containers)
	}
	rec := do(t, srv, http.MethodPost, "/github/runners/pmc-worker-1-org/restart", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("restart status = %d (body %s)", rec.Code, rec.Body.String())
	}
	var out KillOutcome
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Registered {
		t.Fatalf("outcome = %+v, want registered", out)
	}
	if _, ok := h.docker.containers["pmc-worker-1-org"]; !ok {
		t.Fatal("restart did not create the container")
	}
}

func TestListReportsTheOrgAndTheSwitch(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1"})
	h.github.addRunner("acme", "pmc-worker-1-org", "online")
	rec := do(t, newRunnerHTTP(t, h), http.MethodGet, "/github/runners", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Enabled bool `json:"enabled"`
		Runners []struct {
			Name  string `json:"name"`
			Org   string `json:"org"`
			Node  string `json:"node"`
			State string `json:"state"`
		} `json:"runners"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Enabled {
		t.Fatal("enabled = false, want the switch reported alongside the list")
	}
	if len(body.Runners) != 1 || body.Runners[0].Org != "acme" || body.Runners[0].Node != "worker-1" {
		t.Fatalf("runners = %+v", body.Runners)
	}
	if body.Runners[0].State != StateIdle {
		t.Fatalf("state = %q, want idle", body.Runners[0].State)
	}
}

// A malformed confirm body is a 400, not a silent kill.
func TestKillWithAMalformedBodyIs400(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1"})
	h.docker.containers["pmc-worker-1-org"] = Container{ID: "c1", Name: "pmc-worker-1-org", State: "running"}
	rec := do(t, newRunnerHTTP(t, h), http.MethodPost, "/github/runners/pmc-worker-1-org/kill", `{"confirm":"yes"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}
