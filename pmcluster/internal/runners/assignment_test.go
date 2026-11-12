package runners

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth/swarmtls"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth/swarmtls/swarmtlstest"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// ---- resolution: the same rules as the reconcile ----------------------------

// TestResolveAssignmentMatchesTheReconcileRules pins the decision table. Each case
// is one refusal the reconcile already makes, so a node that asks cannot get a
// different answer than the leader's own pass would give.
func TestResolveAssignmentMatchesTheReconcileRules(t *testing.T) {
	workers := []runtime.Node{
		{ID: "a", Hostname: "worker-1", Role: "worker"},
		{ID: "b", Hostname: "worker-2", Role: "worker"},
		{ID: "c", Hostname: "manager-1", Role: "manager", IsLeader: true},
	}
	base := Config{
		Enabled: true, Org: "acme", Group: "pmcluster",
		Nodes: []string{"worker-1"}, Memory: "2560m",
	}

	t.Run("selected worker", func(t *testing.T) {
		a, err := ResolveAssignment(base, workers, "worker-1")
		if err != nil {
			t.Fatalf("unexpected refusal: %v", err)
		}
		if !a.Enabled || a.Org != "acme" || a.Mode != ModeEphemeral || a.Labels != Labels {
			t.Fatalf("assignment = %+v", a)
		}
		if !strings.Contains(a.Reason, "selected") {
			t.Fatalf("reason should say why it is enabled: %q", a.Reason)
		}
	})

	t.Run("unselected worker gets a disabled answer, not an error", func(t *testing.T) {
		a, err := ResolveAssignment(base, workers, "worker-2")
		if err != nil {
			t.Fatalf("an unselected node must get an answer: %v", err)
		}
		if a.Enabled || !strings.Contains(a.Reason, "not in github_runner_nodes") {
			t.Fatalf("assignment = %+v", a)
		}
	})

	t.Run("feature off", func(t *testing.T) {
		cfg := base
		cfg.Enabled = false
		a, err := ResolveAssignment(cfg, workers, "worker-1")
		if err != nil {
			t.Fatalf("unexpected refusal: %v", err)
		}
		if a.Enabled || !strings.Contains(a.Reason, "github_runners_enabled") {
			t.Fatalf("assignment = %+v", a)
		}
	})

	t.Run("empty node list is a refusal", func(t *testing.T) {
		cfg := base
		cfg.Nodes = nil
		if _, err := ResolveAssignment(cfg, workers, "worker-1"); !errors.Is(err, ErrNoNodes) {
			t.Fatalf("err = %v, want ErrNoNodes", err)
		}
	})

	t.Run("empty org is a refusal", func(t *testing.T) {
		cfg := base
		cfg.Org = ""
		if _, err := ResolveAssignment(cfg, workers, "worker-1"); !errors.Is(err, ErrNoOrg) {
			t.Fatalf("err = %v, want ErrNoOrg", err)
		}
	})

	t.Run("a manager in the selection is refused", func(t *testing.T) {
		cfg := base
		cfg.Nodes = []string{"worker-1", "manager-1"}
		if _, err := ResolveAssignment(cfg, workers, "worker-1"); !errors.Is(err, ErrManagerNode) {
			t.Fatalf("err = %v, want ErrManagerNode", err)
		}
	})

	t.Run("the leader selecting itself is refused", func(t *testing.T) {
		cfg := base
		cfg.Nodes = []string{"manager-1"}
		if _, err := ResolveAssignment(cfg, workers, "manager-1"); !errors.Is(err, ErrManagerNode) {
			t.Fatalf("err = %v, want ErrManagerNode", err)
		}
	})
}

// ---- the endpoint -----------------------------------------------------------

// assignmentKeys is the exact response schema. The test asserts equality, so a
// later field cannot be added without this list changing — which is the point:
// nothing else may travel over this channel.
// The exact key set the endpoint may answer with. "held" is the operator's
// console kill carried to the node and "refused" is the memory-size refusal —
// both decisions, not credentials, so they belong here; anything secret or
// settings-shaped does not.
// assignmentKeys is the whole contract between the leader and a worker, pinned key by
// key. There is no docker-access key in it: a job always gets a daemon of its own, so
// there is nothing for the leader to decide and nothing for the worker to obey.
var assignmentKeys = []string{
	"enabled",
	"group", "held", "interval_seconds", "issued_at", "labels", "memory",
	"mode", "node", "node_docker_restart", "org", "reason", "refused", "registration_token",
	"registration_token_expires_at",
}

func newAssignmentService(gh GitHub, nodes []runtime.Node, cfg Config, drained *[]string) *AssignmentService {
	return &AssignmentService{
		Config: func(context.Context) Config { return cfg },
		Nodes:  fakeNodes{nodes: nodes},
		GitHub: gh,
		Deregister: func(_ context.Context, org, name string) error {
			if drained != nil {
				*drained = append(*drained, org+"/"+name)
			}
			return nil
		},
		Now: func() time.Time { return time.Unix(1700000000, 0) },
		Log: zerolog.Nop(),
	}
}

// requestFor builds a request carrying an authenticated identity, exactly as the
// middleware would have left it.
func requestFor(node swarmtls.Node, target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	return r.WithContext(swarmtls.ContextWithNode(r.Context(), node))
}

func TestAssignmentEndpointRefusalsAndAnswer(t *testing.T) {
	workers := []runtime.Node{{ID: "id-w1", Hostname: "worker-1", Role: "worker"}}
	enabled := Config{Enabled: true, Org: "acme", Group: "pmcluster", Nodes: []string{"worker-1"}, Memory: "2560m", IntervalSeconds: 60}

	t.Run("refuses a caller with no identity", func(t *testing.T) {
		svc := newAssignmentService(newFakeGitHub(), workers, enabled, nil)
		rec := httptest.NewRecorder()
		svc.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, AssignmentPath, nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (an unauthenticated read must never be answered)", rec.Code)
		}
	})

	t.Run("refuses a non-GET", func(t *testing.T) {
		svc := newAssignmentService(newFakeGitHub(), workers, enabled, nil)
		rec := httptest.NewRecorder()
		post := httptest.NewRequest(http.MethodPost, AssignmentPath, nil)
		post = post.WithContext(swarmtls.ContextWithNode(post.Context(), swarmtls.Node{ID: "id-w1", Hostname: "worker-1"}))
		svc.ServeHTTP(rec, post)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405 (the channel is read-only)", rec.Code)
		}
	})

	t.Run("refuses a manager in the selection", func(t *testing.T) {
		nodes := []runtime.Node{
			{ID: "id-w1", Hostname: "worker-1", Role: "worker"},
			{ID: "id-m1", Hostname: "manager-1", Role: "manager"},
		}
		cfg := enabled
		cfg.Nodes = []string{"worker-1", "manager-1"}
		svc := newAssignmentService(newFakeGitHub(), nodes, cfg, nil)
		rec := httptest.NewRecorder()
		svc.ServeHTTP(rec, requestFor(swarmtls.Node{ID: "id-w1", Hostname: "worker-1"}, AssignmentPath))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (ErrManagerNode): %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "manager") {
			t.Fatalf("the refusal should name the manager: %s", rec.Body.String())
		}
	})

	t.Run("refuses a group whose visibility is all", func(t *testing.T) {
		gh := newFakeGitHub()
		gh.groupVis = "all"
		svc := newAssignmentService(gh, workers, enabled, nil)
		rec := httptest.NewRecorder()
		svc.ServeHTTP(rec, requestFor(swarmtls.Node{ID: "id-w1", Hostname: "worker-1"}, AssignmentPath))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (visibility refusal): %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "selected") {
			t.Fatalf("the refusal should say what to set: %s", rec.Body.String())
		}
		if len(gh.tokens) != 0 {
			t.Fatal("a refused assignment must not mint a registration token")
		}
	})

	t.Run("answers the selected node, with exactly the documented keys", func(t *testing.T) {
		svc := newAssignmentService(newFakeGitHub(), workers, enabled, nil)
		rec := httptest.NewRecorder()
		svc.ServeHTTP(rec, requestFor(swarmtls.Node{ID: "id-w1", Hostname: "worker-1"}, AssignmentPath))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var raw map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("decode: %v", err)
		}
		var got []string
		for k := range raw {
			got = append(got, k)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(assignmentKeys, ",") {
			t.Fatalf("response keys = %v\nwant %v\nThe assignment carries the runner spec and one single-use registration token — no settings, no secrets, no credentials.", got, assignmentKeys)
		}
		if raw["registration_token"] != "token-acme" {
			t.Fatalf("registration_token = %v, want the minted token", raw["registration_token"])
		}
		if raw["node"] != "worker-1" {
			t.Fatalf("node = %v, want the certificate's node", raw["node"])
		}
		for _, forbidden := range []string{"password", "secret", "private_key", "api_token", "settings", "swarm_id", "credential"} {
			if strings.Contains(strings.ToLower(rec.Body.String()), forbidden) {
				t.Fatalf("the response mentions %q: %s", forbidden, rec.Body.String())
			}
		}
	})

	t.Run("query parameter never selects whose assignment is returned", func(t *testing.T) {
		svc := newAssignmentService(newFakeGitHub(), workers, enabled, nil)
		rec := httptest.NewRecorder()
		svc.ServeHTTP(rec, requestFor(swarmtls.Node{ID: "id-w1", Hostname: "worker-1"},
			AssignmentPath+"?node=manager-1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var got Assignment
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Node != "worker-1" {
			t.Fatalf("node = %q, want the certificate's node (worker-1); a query parameter must not be an identity", got.Node)
		}
	})

	t.Run("a disabled answer asks the leader to de-register, and the worker gets no token", func(t *testing.T) {
		var drained []string
		svc := newAssignmentService(newFakeGitHub(), workers, Config{Enabled: false, Org: "acme"}, &drained)
		rec := httptest.NewRecorder()
		svc.ServeHTTP(rec, requestFor(swarmtls.Node{ID: "id-w1", Hostname: "worker-1"}, AssignmentPath))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var got Assignment
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Enabled || got.RegistrationToken != "" {
			t.Fatalf("a disabled assignment must carry no token: %+v", got)
		}
		if len(drained) != 1 || drained[0] != "acme/pmc-worker-1-org" {
			t.Fatalf("leader-side de-registration = %v, want acme/pmc-worker-1-org", drained)
		}
		// Bounded: a second poll inside the window does not list GitHub again.
		rec2 := httptest.NewRecorder()
		svc.ServeHTTP(rec2, requestFor(swarmtls.Node{ID: "id-w1", Hostname: "worker-1"}, AssignmentPath))
		if len(drained) != 1 {
			t.Fatalf("de-registration attempts = %d after two polls, want 1 (bounded)", len(drained))
		}
	})
}

// TestAssignmentOverRealMTLS is the end-to-end proof: a worker holding its swarm
// certificate reads ITS assignment through the mTLS listener, and the node in the
// answer is the certificate's, not the query string's.
func TestAssignmentOverRealMTLS(t *testing.T) {
	const swarmID = "lm03ez1is3mlvt8zevoc2lfis"
	const workerNodeID = "ppbl900hxr1cyypgvzco2tpeu"

	ca := swarmtlstest.New(t)
	now := time.Now()
	serverPair := ca.Issue(t, swarmID, "manager-node-id", "swarm-manager", now.Add(-time.Hour), now.Add(time.Hour))
	workerPair := ca.Issue(t, swarmID, workerNodeID, "swarm-worker", now.Add(-time.Hour), now.Add(time.Hour))

	nodes := []runtime.Node{
		{ID: workerNodeID, Hostname: "nxt-sw-6-w", Role: "worker"},
		{ID: "manager-node-id", Hostname: "nxt-sw-5-m", Role: "manager", IsLeader: true},
	}
	cfg := Config{Enabled: true, Org: "acme", Group: "pmcluster", Nodes: []string{"nxt-sw-6-w"}, Memory: "2560m", IntervalSeconds: 60}

	svc := &AssignmentService{
		Config: func(context.Context) Config { return cfg },
		Nodes:  fakeNodes{nodes: nodes},
		GitHub: newFakeGitHub(),
		Log:    zerolog.Nop(),
	}
	verifier := &swarmtls.Verifier{
		CA:      ca.Pool,
		Nodes:   fakeNodes{nodes: nodes},
		SwarmID: func(context.Context) (string, error) { return swarmID, nil },
		Log:     zerolog.Nop(),
	}
	mux := http.NewServeMux()
	// The single mount: one route, wrapped by the verifier.
	mux.Handle(AssignmentPath, verifier.Middleware(svc))

	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverPair.Pair},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.Pool,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	defer srv.Close()

	dir := t.TempDir()
	certFile, keyFile := swarmtlstest.WriteNode(t, dir, "worker", workerPair)
	caFile := ca.WriteCA(t, dir)
	clientTLS, err := swarmtls.ClientTLS(swarmtls.ClientConfig{CertFile: certFile, KeyFile: keyFile, CA: ca.Pool, SwarmID: swarmID})
	if err != nil {
		t.Fatalf("ClientTLS: %v", err)
	}
	hostPort := strings.TrimPrefix(srv.URL, "https://")
	_, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		t.Fatalf("split the test server address: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse the test server port: %v", err)
	}
	client := &Client{
		CertFile: certFile,
		KeyFile:  keyFile,
		CAPaths:  []string{caFile},
		Port:     port,
		Managers: func(context.Context) ([]string, error) { return []string{hostPort}, nil },
		HTTP:     &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}, Timeout: 5 * time.Second},
		Log:      zerolog.Nop(),
	}
	a, source, err := client.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if a.Node != "nxt-sw-6-w" || !a.Enabled || a.Org != "acme" {
		t.Fatalf("assignment = %+v (source %s)", a, source)
	}
	if a.RegistrationToken == "" {
		t.Fatal("an enabled assignment must carry the registration token the container registers with")
	}

	// The same client without a certificate does not poll at all.
	none := &Client{CertFile: dir + "/absent.crt", KeyFile: dir + "/absent.key", Log: zerolog.Nop()}
	if _, _, err := none.Fetch(context.Background()); !errors.Is(err, ErrNoSwarmCertificate) {
		t.Fatalf("err = %v, want ErrNoSwarmCertificate (no unauthenticated fallback exists)", err)
	}
}

// TestClientWithoutSwarmCertificate covers the non-swarm box: one sentinel, no
// poll, and no unauthenticated fallback.
func TestClientWithoutSwarmCertificate(t *testing.T) {
	dir := t.TempDir()
	c := &Client{CertFile: dir + "/missing.crt", KeyFile: dir + "/missing.key", Log: zerolog.Nop()}
	if _, _, err := c.Fetch(context.Background()); !errors.Is(err, ErrNoSwarmCertificate) {
		t.Fatalf("err = %v, want ErrNoSwarmCertificate", err)
	}
}

// ---- the worker's agent -----------------------------------------------------

// TestAgentAppliesEnabledAssignment is the requirement in one test: the worker
// starts its runner from the assignment, with the filtered socket and no raw mount.
func TestAgentAppliesEnabledAssignment(t *testing.T) {
	docker := newFakeDocker()
	dir := t.TempDir()
	agent := &Agent{
		Node:      "nxt-sw-6-w",
		Docker:    docker,
		StatePath: dir + "/runner_assignment.json",
		Log:       zerolog.Nop(),
	}
	a := Assignment{
		Node: "nxt-sw-6-w", Enabled: true, Org: "acme", Group: "pmcluster",
		Memory: "2560m", Mode: ModeEphemeral, Labels: Labels,
		Reason: "selected as a runner node", RegistrationToken: "reg-token",
	}
	res, err := agent.Apply(context.Background(), a)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Registered) != 1 || res.Registered[0] != "acme" {
		t.Fatalf("registered = %v", res.Registered)
	}
	// Two containers now: the daemon, then the runner. The job's docker access is
	// not optional, so every start is a dind start.
	if len(docker.runCalls) != 2 {
		t.Fatalf("docker run calls = %d, want 2 (daemon + runner)", len(docker.runCalls))
	}
	argv := strings.Join(docker.runCalls[len(docker.runCalls)-1], " ")
	// The path in DOCKER_HOST is the runner's own daemon, reached through a volume.
	// What must not appear is a MOUNT of any host socket — the old filtered proxy's
	// directory or the host's raw socket.
	if strings.Contains(argv, "-v /var/run/docker.sock") || strings.Contains(argv, "/run/pmcluster") {
		t.Fatalf("the worker's runner mounts a host socket: %s", argv)
	}
	if !strings.Contains(argv, "-v pmc-nxt-sw-6-w-org-docker:/var/run") {
		t.Fatalf("the worker's runner does not share its daemon's socket: %s", argv)
	}
	// And the caches are mounted, or every job would re-download its dependencies.
	for _, want := range []string{"-cache-go:/go-cache", "-cache-npm:/npm-cache", "GOMODCACHE=/go-cache/mod", "npm_config_cache=/npm-cache/npm"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("the runner does not mount its caches (%s missing): %s", want, argv)
		}
	}
	if !strings.Contains(argv, "RUNNER_TOKEN=reg-token") || !strings.Contains(argv, "RUNNER_NAME=pmc-nxt-sw-6-w-org") {
		t.Fatalf("argv = %s", argv)
	}

	// The bookkeeping the doctor reads on a worker, written 0600.
	rec, ok := agent.Record()
	if !ok || rec.Container != "running" || !rec.Enabled || rec.Org != "acme" {
		t.Fatalf("record = %+v ok=%v", rec, ok)
	}
	info, err := os.Stat(agent.StatePath)
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file mode = %o, want 0600", perm)
	}
	if b, _ := os.ReadFile(agent.StatePath); strings.Contains(string(b), "reg-token") {
		t.Fatal("the registration token was written to disk")
	}

	// A second pass with a running container does nothing: a fresh single-use token
	// must not be burned on every poll.
	if _, err := agent.Apply(context.Background(), a); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if len(docker.runCalls) != 2 {
		t.Fatalf("docker run calls = %d after a second pass, want 2 (a second pass registers nothing new)", len(docker.runCalls))
	}
}

// TestAgentStopsOnDisabledAssignment: enabled=false is the teardown path.
func TestAgentStopsOnDisabledAssignment(t *testing.T) {
	docker := newFakeDocker()
	dir := t.TempDir()
	agent := &Agent{Node: "nxt-sw-6-w", Docker: docker, StatePath: dir + "/runner_assignment.json", Log: zerolog.Nop()}
	if _, err := agent.Apply(context.Background(), Assignment{
		Node: "nxt-sw-6-w", Enabled: true, Org: "acme", Group: "pmcluster", Memory: "2560m",
		RegistrationToken: "reg-token",
	}); err != nil {
		t.Fatalf("Apply enabled: %v", err)
	}
	if _, ok, _ := docker.Inspect(context.Background(), "pmc-nxt-sw-6-w-org"); !ok {
		t.Fatal("setup: the runner container should exist")
	}

	res, err := agent.Apply(context.Background(), Assignment{
		Node: "nxt-sw-6-w", Enabled: false, Org: "acme", Reason: "this node is not in github_runner_nodes",
	})
	if err != nil {
		t.Fatalf("Apply disabled: %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != "pmc-nxt-sw-6-w-org" {
		t.Fatalf("removed = %v", res.Removed)
	}
	if _, ok, _ := docker.Inspect(context.Background(), "pmc-nxt-sw-6-w-org"); ok {
		t.Fatal("the runner container is still there after a disabled assignment")
	}
	rec, ok := agent.Record()
	if !ok || rec.Enabled || rec.Container != "absent" {
		t.Fatalf("record = %+v ok=%v", rec, ok)
	}
}

// TestAgentEnabledWithoutTokenFailsLoudly: an enabled answer with no token would
// start a container that 401s inside the runner and looks like a workflow bug.
func TestAgentEnabledWithoutTokenFailsLoudly(t *testing.T) {
	docker := newFakeDocker()
	agent := &Agent{Node: "w1", Docker: docker, Log: zerolog.Nop()}
	_, err := agent.Apply(context.Background(), Assignment{
		Node: "w1", Enabled: true, Org: "acme", Group: "pmcluster", Memory: "2560m",
	})
	if err == nil || !strings.Contains(err.Error(), "registration token") {
		t.Fatalf("err = %v, want a missing-token refusal", err)
	}
	if len(docker.runCalls) != 0 {
		t.Fatal("a container was started without a token")
	}
}

// ---- assignment tuning ------------------------------------------------------

func TestAssignmentIntervalIsClamped(t *testing.T) {
	cases := map[int]time.Duration{
		0:     DefaultAssignmentIntervalSeconds * time.Second,
		-5:    DefaultAssignmentIntervalSeconds * time.Second,
		1:     10 * time.Second,
		60:    60 * time.Second,
		99999: time.Hour,
	}
	for in, want := range cases {
		if got := (Assignment{IntervalSeconds: in}).Interval(); got != want {
			t.Errorf("IntervalSeconds=%d → %s, want %s", in, got, want)
		}
	}
}

// TestAssignmentConfigCarriesNoDockerDecision: the spec is built from the assignment,
// and there is exactly one way for a job to reach docker now — a daemon of its own. A
// field that let a worker choose a socket would be a knob with one honest position.
func TestAssignmentConfigCarriesNoDockerDecision(t *testing.T) {
	a := Assignment{Node: "w1", Enabled: true, Org: "acme", Group: "g", Memory: "1g"}
	cfg := a.Config("w1")
	if cfg.Memory != "1g" || cfg.Org != "acme" || !cfg.Enabled {
		t.Fatalf("config = %+v", cfg)
	}
	spec := Spec{Org: cfg.Org, RunnerName: "pmc-w1-acme", Memory: cfg.Memory, Token: "t"}
	argv, err := spec.Argv()
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, spec.DinDVolume()+":/var/run") {
		t.Fatalf("the runner does not mount its daemon's socket volume: %v", argv)
	}
	if strings.Contains(joined, "/var/run/docker.sock:/var/run/docker.sock") {
		t.Fatalf("the host docker socket is mounted: %v", argv)
	}
}

// TestAgentRemovesARunnerHeldByAnOperator: a kill from the console runs on the
// leader, which cannot remove a container on a worker. The hold travels in the
// assignment, and the node acts on it — otherwise the button reports success
// while the node recreates the runner seconds later.
func TestAgentRemovesARunnerHeldByAnOperator(t *testing.T) {
	docker := newFakeDocker()
	dir := t.TempDir()
	agent := &Agent{
		Node:      "nxt-sw-6-w",
		Docker:    docker,
		StatePath: dir + "/runner_assignment.json",
		Log:       zerolog.Nop(),
	}
	// First pass: the node is running the runner normally.
	live := Assignment{
		Node: "nxt-sw-6-w", Enabled: true, Org: "acme", Group: "pmcluster",
		Memory: "2560m", Mode: ModeEphemeral, Labels: Labels,
		Reason: "selected as a runner node", RegistrationToken: "reg-token",
	}
	if _, err := agent.Apply(context.Background(), live); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	runCalls := len(docker.runCalls)

	// Second pass: an operator killed it on the leader.
	held := live
	held.Held = true
	held.Reason = "stopped by an operator from the console"
	res, err := agent.Apply(context.Background(), held)
	if err != nil {
		t.Fatalf("held apply: %v", err)
	}
	if len(docker.runCalls) != runCalls {
		t.Fatalf("a held runner was started again: run calls %d -> %d", runCalls, len(docker.runCalls))
	}
	if len(res.Removed) != 1 {
		t.Fatalf("removed = %v, want the held container removed", res.Removed)
	}
	rec, ok := agent.Record()
	if !ok || rec.Container != "held" {
		t.Fatalf("record = %+v ok=%v, want container=held", rec, ok)
	}

	// Third pass: the hold is the same answer repeated, and stays quiet.
	if _, err := agent.Apply(context.Background(), held); err != nil {
		t.Fatalf("repeat held apply: %v", err)
	}
	if len(docker.runCalls) != runCalls {
		t.Fatalf("a held runner was restarted on a later pass")
	}
}

// TestCheckPlacementHonoursAllowLeader is the regression for a switch that existed and
// did nothing: github_runner_allow_leader was read into Config, carried to the placement
// check, and never consulted — so the console offered the toggle and the daemon refused
// the leader anyway.
func TestCheckPlacementHonoursAllowLeader(t *testing.T) {
	nodes := []runtime.Node{
		{Hostname: "nxt-sw-5-m", Role: "manager", IsLeader: true},
		{Hostname: "nxt-sw-1-m", Role: "manager"},
		{Hostname: "nxt-sw-6-w", Role: "worker"},
	}

	// Off: the leader is refused, as it always was.
	off := Config{Nodes: []string{"nxt-sw-5-m"}}
	if err := CheckPlacement(off, nodes); !errors.Is(err, ErrManagerNode) {
		t.Fatalf("with allow-leader off, err = %v, want ErrManagerNode", err)
	}

	// On: the leader is allowed.
	on := Config{Nodes: []string{"nxt-sw-5-m"}, AllowManagers: true}
	if err := CheckPlacement(on, nodes); err != nil {
		t.Fatalf("with allow-leader on, the leader was still refused: %v", err)
	}

	// On covers EVERY manager, not only the leader. Keying it to the leader was the
	// bug the operator reported: the console offered the switch, and the other
	// managers stayed unselectable for a reason no one could see.
	other := Config{Nodes: []string{"nxt-sw-1-m"}, AllowManagers: true}
	if err := CheckPlacement(other, nodes); err != nil {
		t.Fatalf("allow-managers refused a non-leader manager: %v", err)
	}

	// A worker is unaffected either way.
	worker := Config{Nodes: []string{"nxt-sw-6-w"}, AllowManagers: true}
	if err := CheckPlacement(worker, nodes); err != nil {
		t.Fatalf("a worker was refused: %v", err)
	}
}

// TestRolesOfTellsAllowedFromRefused: the picker has three row states — pickable,
// pickable-with-a-cost, refused — and the leader with the switch on is the middle one.
func TestRolesOfTellsAllowedFromRefused(t *testing.T) {
	leader := runtime.Node{Hostname: "nxt-sw-5-m", Role: "manager", IsLeader: true}

	ok, note := RolesOf(leader, false)
	if ok {
		t.Fatal("the leader was selectable with allow-leader off")
	}
	if !strings.Contains(note, "Allow runners on managers") {
		t.Fatalf("the refusal does not name the switch that lifts it: %q", note)
	}

	ok, note = RolesOf(leader, true)
	if !ok {
		t.Fatal("the leader was refused with allow-leader on")
	}
	if !strings.Contains(note, "competes with the API") {
		t.Fatalf("the allowed leader carries no caution: %q", note)
	}

	if ok, _ := RolesOf(runtime.Node{Hostname: "w", Role: "worker"}, false); !ok {
		t.Fatal("a worker was refused")
	}
}
