//go:build e2e

// The observability alerts case: the real daemon binary, its real Bearer
// authentication, its real Parseable adapter and its read cache, driven over
// HTTP exactly as the console and MCP do.
//
// WHY A PARSEABLE-SHAPED STUB AND NOT A DEPLOYED PARSEABLE: the assertion this
// test exists for is "two identical reads cost ONE upstream fan-out". Counting
// fan-outs needs an upstream that can count them, and the swarm e2e job has no
// way to ask a deployed Parseable how many requests it answered. The daemon
// resolves its read endpoint from the swarm task and falls back to
// 127.0.0.1:<ObservabilityPort>, which is what the console and the MCP endpoint
// would use on a real cluster; the stub binds exactly that address, so the code
// under test is unchanged and the only substitution is the far end.
//
// The daemon runs in its documented no-engine (standalone) mode so the read
// endpoint is deterministic no matter what the surrounding swarm has deployed:
// deployment and task resolution are TestClusterUp's job, not this one's.
//
// If a real observability stack owns that port, the case skips with the reason
// instead of pretending: the daemon would then read a different backend and a
// zero count would be meaningless rather than wrong.

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// alertsStubUpstream is a Parseable-shaped endpoint that records how many alert
// reads it answered, so the cache can be proven by counting rather than by
// timing.
type alertsStubUpstream struct {
	mu       sync.Mutex
	reads    int
	lastAuth string
	body     string
}

func (u *alertsStubUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v1/alerts" {
		http.NotFound(w, r)
		return
	}
	u.mu.Lock()
	u.reads++
	u.lastAuth = r.Header.Get("Authorization")
	body := u.body
	u.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

func (u *alertsStubUpstream) snapshot() (reads int, auth string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.reads, u.lastAuth
}

// alertsResponse is the documented envelope from openapi.yaml
// (ObservabilityAlerts): status, alerts, optional error.
type alertsResponse struct {
	Status string           `json:"status"`
	Alerts []map[string]any `json:"alerts"`
	Error  string           `json:"error"`
}

// TestObservabilityAlertsE2E proves the alerts route end to end against the
// real daemon: authentication, the documented envelope, the 500 bound, and the
// cache collapsing identical reads into one upstream fan-out.
func TestObservabilityAlertsE2E(t *testing.T) {
	if os.Getenv("PMCLUSTER_E2E_SWARM") == "0" {
		t.Skip("PMCLUSTER_E2E_SWARM=0: e2e explicitly disabled on this machine")
	}

	// Bind the address the daemon actually dials. A bind failure means a real
	// observability stack owns it, so the daemon would read THAT and the counter
	// below would never see the requests: say so and skip rather than assert a
	// zero that means nothing.
	port := cluster.ObservabilityPort
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Skipf("cannot bind 127.0.0.1:%d, the daemon's observability read endpoint: %v", port, err)
	}
	upstream := &alertsStubUpstream{body: `[
		{"name":"disk-full","state":"firing","message":"node 1 is above 90%"},
		{"name":"no-backups","state":"ok","message":"the last backup is 2h old"},
		{"name":"cert-expiry","state":"pending","message":"the site cert expires in 12d"}
	]`}
	stub := &http.Server{Handler: upstream, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = stub.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stub.Shutdown(ctx)
	})

	homeDir := t.TempDir()
	stdout, _, code := runCmd(t, homeDir, "init")
	if code != 0 {
		t.Fatalf("pmcluster init exited %d:\n%s", code, stdout)
	}
	adminToken := extractToken(t, stdout)

	// The daemon reads Parseable with the managed credential it was bootstrapped
	// with. `cluster up` normally mints it; here the store is seeded directly,
	// because this case needs no cluster — only the credential the read path
	// would find on a bootstrapped one. The reconcile interval is pinned in the
	// same pass so the control loop cannot race this test's reads (the CLI
	// helper would need a leader, and this case may run with no Docker at all).
	const upstreamUser, upstreamPass = "e2e@example.test", "e2e-parseable-password"
	seedObservabilityStore(t, homeDir, upstreamUser, upstreamPass)

	addr := freePort(t)
	var serveLog bytes.Buffer
	serveCmd := exec.Command(binaryPath, "serve")
	serveCmd.Stdout = &serveLog
	serveCmd.Stderr = &serveLog
	// Standalone: an invalid DOCKER_HOST makes docker.New() fail, which is the
	// daemon's documented "no engine" path — it serves inventory-only and the
	// read endpoint falls back to 127.0.0.1:<ObservabilityPort>, the address the
	// stub holds. That keeps the case independent of whatever stacks the
	// surrounding swarm has deployed (placement is TestClusterUp's job) and is
	// why the counter is meaningful here. Duplicate DOCKER_* keys are stripped
	// first: on Unix getenv returns the FIRST match, so appending blindly would
	// silently keep an inherited value.
	serveCmd.Env = append(withoutEnv(homeEnv(homeDir), "DOCKER_HOST", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_CONTEXT"),
		"DOCKER_HOST=tcp://", // unparseable on purpose: no engine for this daemon
		"PMCLUSTER_LISTEN_ADDR="+addr,
	)
	if err := serveCmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	t.Cleanup(func() {
		if serveCmd.Process == nil {
			return
		}
		_ = serveCmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = serveCmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(e2eTimeout(10 * time.Second)):
			_ = serveCmd.Process.Kill()
		}
		if t.Failed() {
			t.Logf("daemon log:\n%s", tail(serveLog.String(), 4000))
		}
	})
	waitHealthy(t, addr, serveCmd, e2eTimeout(15*time.Second))

	base := "http://" + addr
	client := &http.Client{Timeout: e2eTimeout(15 * time.Second)}

	alertsGet := func(t *testing.T, limit int) (*http.Response, alertsResponse) {
		t.Helper()
		url := base + "/api/observability/alerts?limit=" + strconv.Itoa(limit)
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var body alertsResponse
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("GET %s: response is not the documented JSON envelope: %v (%s)", url, err, raw)
			}
		}
		return resp, body
	}

	t.Run("unauthenticated request is refused", func(t *testing.T) {
		resp, err := client.Get(base + "/api/observability/alerts?limit=10")
		if err != nil {
			t.Fatalf("GET alerts without a token: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status without a token = %d, want 401", resp.StatusCode)
		}
		if reads, _ := upstream.snapshot(); reads != 0 {
			t.Fatalf("an unauthenticated request reached the upstream %d times", reads)
		}
	})

	t.Run("documented shape and the requested bound", func(t *testing.T) {
		resp, body := alertsGet(t, 2)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		// Either the backend answered or it is documented as unavailable; the
		// daemon must never turn a backend problem into a 500.
		switch body.Status {
		case "available":
		case "unavailable":
			t.Fatalf("the stub upstream is healthy but the daemon reports unavailable (error=%q) — the read path is broken", body.Error)
		default:
			t.Fatalf("status = %q, want available (or unavailable)", body.Status)
		}
		if body.Alerts == nil {
			t.Fatal("alerts is null; the documented shape is an array")
		}
		if len(body.Alerts) != 2 {
			t.Fatalf("alerts = %d, want 2 (the stub serves 3, the requested limit is 2)", len(body.Alerts))
		}
		for i, row := range body.Alerts {
			if row["name"] == nil || row["name"] == "" {
				t.Errorf("alert %d has no name: %+v", i, row)
			}
		}
		if reads, auth := upstream.snapshot(); reads != 1 {
			t.Errorf("upstream reads = %d after one request, want 1", reads)
		} else if !strings.HasPrefix(auth, "Basic ") {
			// The daemon authenticates with its managed credential; a stub that
			// never sees one cannot prove the auth path ran.
			t.Errorf("upstream saw Authorization %q, want the managed Basic credential", auth)
		}
	})

	t.Run("row count never exceeds the requested limit", func(t *testing.T) {
		for _, limit := range []int{1, 2, 3, 5} {
			resp, body := alertsGet(t, limit)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("limit %d status = %d, want 200", limit, resp.StatusCode)
			}
			if len(body.Alerts) > limit {
				t.Fatalf("limit %d returned %d alerts; the daemon must never exceed the caller's bound", limit, len(body.Alerts))
			}
		}
	})

	t.Run("identical back-to-back requests cause ONE upstream fan-out", func(t *testing.T) {
		// A limit no earlier subtest used, so the key is definitely cold.
		const limit = 17
		before, _ := upstream.snapshot()

		_, first := alertsGet(t, limit)
		if len(first.Alerts) != 3 {
			t.Fatalf("first read returned %d alerts, want 3", len(first.Alerts))
		}
		afterFirst, _ := upstream.snapshot()
		if afterFirst != before+1 {
			t.Fatalf("upstream reads after the first request = %d, want %d", afterFirst, before+1)
		}

		_, second := alertsGet(t, limit)
		if len(second.Alerts) != len(first.Alerts) {
			t.Fatalf("the cached read returned %d alerts, the miss returned %d", len(second.Alerts), len(first.Alerts))
		}
		if afterSecond, _ := upstream.snapshot(); afterSecond != afterFirst {
			t.Fatalf("the second identical request reached the upstream (%d reads, want %d): the 10s read cache is not in the daemon path", afterSecond, afterFirst)
		}

		// A different bound is a different key: it must reach the upstream.
		alertsGet(t, limit+1)
		if afterDifferent, _ := upstream.snapshot(); afterDifferent != afterFirst+1 {
			t.Fatalf("a different limit did not reach the upstream (%d reads, want %d)", afterDifferent, afterFirst+1)
		}
	})

	t.Run("limit over the cap is refused, not clamped silently", func(t *testing.T) {
		before, _ := upstream.snapshot()
		resp, _ := alertsGet(t, 5000)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("limit 5000 status = %d, want the documented 400", resp.StatusCode)
		}
		if reads, _ := upstream.snapshot(); reads != before {
			t.Fatalf("an over-cap limit reached the upstream (%d reads, want %d)", reads, before)
		}
	})
}

// withoutEnv returns env with every key in drop removed, so a value appended
// afterwards is the one the child process actually reads.
func withoutEnv(env []string, drop ...string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		skip := false
		for _, d := range drop {
			if key == d {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, entry)
		}
	}
	return out
}

// seedObservabilityStore writes the managed credential the daemon's read path
// authenticates with, the way `cluster up` would, and pins the reconcile
// interval. It runs before the daemon starts, so the SQLite store is opened and
// closed here.
func seedObservabilityStore(t *testing.T, homeDir, username, password string) {
	t.Helper()
	dataDir := filepath.Join(homeDir, ".pmcluster")

	cipher, err := credentials.Open(filepath.Join(dataDir, ".encryption_key"))
	if err != nil {
		t.Fatalf("open encryption key: %v", err)
	}
	ciphertext, err := cipher.Encrypt([]byte(password))
	if err != nil {
		t.Fatalf("encrypt credential: %v", err)
	}

	st, err := store.Open(filepath.Join(dataDir, "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	}()

	ctx := context.Background()
	if err := st.SetSetting(ctx, cluster.SettingReconcileInterval(), "0"); err != nil {
		t.Fatalf("pin reconcile_interval: %v", err)
	}
	if err := st.InsertCredential(ctx, &store.ManagedCredential{
		Name:               "observability_admin",
		Kind:               string(cluster.KindObservability),
		Username:           username,
		PasswordCiphertext: ciphertext,
		SwarmSecretName:    "observability_admin_password",
	}); err != nil {
		t.Fatalf("insert observability credential: %v", err)
	}
}
