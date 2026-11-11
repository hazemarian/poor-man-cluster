package runners

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// ---- rendered container -----------------------------------------------------

// TestArgvMountsTheFilteredSocketAndNoneOther is the whole point of the socket
// proxy: the runner must not be able to reach /var/run/docker.sock.
func TestArgvMountsTheFilteredSocketAndNoneOther(t *testing.T) {
	argv, err := Spec{
		Org:         "acme",
		Group:       "pmcluster",
		RunnerName:  "pmc-worker-1-org",
		Memory:      "2560m",
		Token:       "reg-token",
		SocketProxy: true,
		SocketPath:  "/run/pmcluster/docker.sock",
	}.Argv()
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "/var/run/docker.sock") {
		t.Fatalf("argv mounts the host socket: %s", joined)
	}
	if !strings.Contains(joined, "DOCKER_HOST=unix:///run/pmcluster/docker.sock") {
		t.Fatalf("argv has no DOCKER_HOST pointing at the filtered socket: %s", joined)
	}
	// The DIRECTORY is mounted, not the socket: a file bind-mount pins an inode
	// the daemon replaces on every restart, which left running runners unable to
	// reach docker after an upgrade.
	if !strings.Contains(joined, "-v /run/pmcluster:/run/pmcluster") {
		t.Fatalf("argv does not mount the filtered socket's directory: %s", joined)
	}
	for _, want := range []string{
		"ORG_NAME=acme",
		"RUNNER_GROUP=pmcluster",
		"RUNNER_TOKEN=reg-token",
		"RUNNER_NAME=pmc-worker-1-org",
		"LABELS=" + Labels,
		"EPHEMERAL=true",
		"--memory 2560m",
		"--restart no",
		"--label " + LabelRunner + "=true",
		"--label " + LabelOrg + "=acme",
		"--label " + LabelName + "=pmc-worker-1-org",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv is missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "REPO_URL") {
		t.Errorf("argv is repo-scoped (REPO_URL): the registration must be org level: %s", joined)
	}
	if argv[len(argv)-1] != DefaultImage {
		t.Errorf("argv ends with %q, want the image", argv[len(argv)-1])
	}
}

// The raw mount still exists as a documented debug fallback — but it has to be
// asked for, and the spec says so.
func TestArgvDebugFallbackMountsTheRawSocket(t *testing.T) {
	argv, err := Spec{Org: "acme", RunnerName: "pmc-worker-1-org", Token: "t", SocketProxy: false}.Argv()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "/var/run/docker.sock:/var/run/docker.sock") {
		t.Fatalf("the debug fallback does not mount the raw socket: %s", joined)
	}
	if !(Spec{Org: "acme", RunnerName: "n", Token: "t", SocketProxy: false}).UsesRawSocket() {
		t.Fatal("Spec.UsesRawSocket does not report the raw mount")
	}
}

func TestRunnerNameIsStableAndSaysTheScope(t *testing.T) {
	name := RunnerName("nxt-sw-3-w")
	if name != "pmc-nxt-sw-3-w-org" {
		t.Fatalf("runner name = %q, want pmc-<node>-org", name)
	}
	if name != RunnerName("nxt-sw-3-w") {
		t.Fatal("runner name is not deterministic")
	}
	long := RunnerName(strings.Repeat("very-long-hostname-", 8))
	if len(long) > 64 {
		t.Fatalf("runner name %q is longer than GitHub's 64-character limit", long)
	}
	// The operator's hand-made runner is NOT this platform's name: the doctor
	// relies on the difference to call it unmanaged.
	if RunnerName("nxt-sw-3-w") == "pmc-org-runner" {
		t.Fatal("the platform's name collides with the hand-made runner")
	}
}

func TestValidation(t *testing.T) {
	if err := ValidateOrg("acme"); err != nil {
		t.Errorf("acme refused: %v", err)
	}
	for _, bad := range []string{"", "acme/api", "-acme", "ac me"} {
		if err := ValidateOrg(bad); err == nil {
			t.Errorf("ValidateOrg(%q) accepted", bad)
		}
	}
	if err := ValidateGroup("pmcluster"); err != nil {
		t.Errorf("pmcluster refused: %v", err)
	}
	for _, bad := range []string{"", "a/b", strings.Repeat("x", 65)} {
		if err := ValidateGroup(bad); err == nil {
			t.Errorf("ValidateGroup(%q) accepted", bad)
		}
	}
	for _, ok := range []string{"2560m", "3g", "512M", "1073741824"} {
		if err := ValidateMemory(ok); err != nil {
			t.Errorf("ValidateMemory(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "lots", "0", "-1g", "256 m"} {
		if err := ValidateMemory(bad); err == nil {
			t.Errorf("ValidateMemory(%q) accepted", bad)
		}
	}
	if got := ParseNodes(" worker-2 , worker-1 ,worker-2, "); len(got) != 2 || got[0] != "worker-2" || got[1] != "worker-1" {
		t.Fatalf("ParseNodes = %v, want order preserved and deduped", got)
	}
	if _, err := NormalizeNodes(""); err == nil {
		t.Fatal("NormalizeNodes accepted an empty list (an enabled feature with no node runs nowhere)")
	}
}

// ---- fakes -----------------------------------------------------------------

type fakeDocker struct {
	containers map[string]Container
	runCalls   [][]string
	removed    []string
	runErr     error
}

func newFakeDocker() *fakeDocker { return &fakeDocker{containers: map[string]Container{}} }

func (f *fakeDocker) Run(_ context.Context, argv []string) error {
	if f.runErr != nil {
		return f.runErr
	}
	f.runCalls = append(f.runCalls, argv)
	name := ""
	binds := []string{}
	for i, a := range argv {
		if a == "--name" && i+1 < len(argv) {
			name = argv[i+1]
		}
		if a == "-v" && i+1 < len(argv) {
			binds = append(binds, argv[i+1])
		}
	}
	f.containers[name] = Container{ID: "id-" + name, Name: name, State: "running", Binds: binds}
	return nil
}

func (f *fakeDocker) Remove(_ context.Context, name string) error {
	delete(f.containers, name)
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeDocker) Inspect(_ context.Context, name string) (Container, bool, error) {
	c, ok := f.containers[name]
	return c, ok, nil
}

type fakeNodes struct{ nodes []runtime.Node }

func (f fakeNodes) NodeList(context.Context) ([]runtime.Node, error) { return f.nodes, nil }

// fakeGitHub is the App surface, in memory. It records every call so the tests
// can count registrations and de-registrations instead of trusting prose.
type fakeGitHub struct {
	tokens      []string
	tokenErr    error
	groupVis    string // visibility of the existing group; "" means absent
	groupEnsure int
	groupRepoID []int64
	runners     map[string][]githubapp.Runner
	listErr     error
	deleteErr   error
	deleted     []string
	perms       map[string]string
	nextID      int64
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		runners: map[string][]githubapp.Runner{},
		perms:   map[string]string{"administration": "write"},
	}
}

func (f *fakeGitHub) RunnerRegistrationToken(_ context.Context, org string) (string, error) {
	if f.tokenErr != nil {
		return "", f.tokenErr
	}
	f.tokens = append(f.tokens, org)
	return "token-" + org, nil
}

func (f *fakeGitHub) ListRunners(_ context.Context, org string) ([]githubapp.Runner, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.runners[org], nil
}

func (f *fakeGitHub) DeleteRunner(_ context.Context, org string, id int64) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, fmt.Sprintf("%s#%d", org, id))
	for i, r := range f.runners[org] {
		if r.ID == id {
			f.runners[org] = append(f.runners[org][:i], f.runners[org][i+1:]...)
			break
		}
	}
	return nil
}

func (f *fakeGitHub) EnsureRunnerGroup(_ context.Context, org, group string) (githubapp.RunnerGroup, error) {
	f.groupEnsure++
	if f.groupVis != "" {
		if f.groupVis != githubapp.GroupVisibilitySelected {
			return githubapp.RunnerGroup{ID: 1, Name: group, Visibility: f.groupVis}, fmt.Errorf("%w: %s", githubapp.ErrRunnerGroupVisibility, f.groupVis)
		}
		return githubapp.RunnerGroup{ID: 1, Name: group, Visibility: f.groupVis}, nil
	}
	// Absent: the production adapter creates it `selected` with the org's private
	// repos. The fake does the same, and records the call.
	return githubapp.RunnerGroup{ID: 2, Name: group, Visibility: githubapp.GroupVisibilitySelected}, nil
}

func (f *fakeGitHub) LatestRunURL(context.Context, string) (string, string, error) {
	return "", "", nil
}

func (f *fakeGitHub) GrantedPermissions(context.Context) (map[string]string, error) {
	return f.perms, nil
}

func (f *fakeGitHub) addRunner(org, name, status string) int64 {
	f.nextID++
	f.runners[org] = append(f.runners[org], githubapp.Runner{ID: f.nextID, Name: name, Status: status})
	return f.nextID
}

// ---- harness ---------------------------------------------------------------

type harness struct {
	store  *store.Store
	github *fakeGitHub
	docker *fakeDocker
	mgr    *Manager
	ctx    context.Context
}

type harnessOpts struct {
	node     string
	enabled  bool
	org      string
	group    string
	nodes    string
	proxy    bool
	managerN bool
}

func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	if o.node == "" {
		o.node = "worker-1"
	}
	if o.group == "" {
		o.group = "pmcluster"
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	settings := map[string]string{
		cluster.SettingGitHubRunnersEnabled(): fmt.Sprintf("%t", o.enabled),
		cluster.SettingGitHubRunnerOrg():      o.org,
		cluster.SettingGitHubRunnerGroup():    o.group,
		cluster.SettingGitHubRunnerNodes():    o.nodes,
		cluster.SettingGitHubRunnerMemory():   "2560m",
	}
	if o.enabled {
		// The settings adapter refuses enabled-without-App; the harness writes
		// the store directly to be able to test the reconcile's own refusal.
		settings[cluster.SettingGitHubRunnerSocketProxy()] = fmt.Sprintf("%t", o.proxy)
	}
	for k, v := range settings {
		if err := st.SetSetting(ctx, k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	nodes := []runtime.Node{
		{Hostname: "worker-1", Role: "worker"},
		{Hostname: "worker-2", Role: "worker"},
	}
	if o.managerN {
		nodes = append(nodes, runtime.Node{Hostname: "manager-1", Role: "manager", IsLeader: true})
	}
	gh := newFakeGitHub()
	dk := newFakeDocker()
	return &harness{
		store:  st,
		github: gh,
		docker: dk,
		ctx:    ctx,
		mgr: &Manager{
			Store:  st,
			GitHub: gh,
			Docker: dk,
			Nodes:  fakeNodes{nodes: nodes},
			Node:   o.node,
			Log:    zerolog.Nop(),
		},
	}
}

// ---- lifecycle -------------------------------------------------------------

func TestSelectedNodeRunsOneRunnerAndRegistersAtOrgLevel(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1"})
	res, err := h.mgr.Reconcile(h.ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %v", res.Warnings)
	}
	if len(h.docker.runCalls) != 1 {
		t.Fatalf("docker run called %d times, want exactly one runner on this node", len(h.docker.runCalls))
	}
	if len(h.github.tokens) != 1 || h.github.tokens[0] != "acme" {
		t.Fatalf("registration tokens = %v, want one minted for the ORG", h.github.tokens)
	}
	if h.github.groupEnsure != 1 {
		t.Fatalf("the runner group was not verified exactly once (%d)", h.github.groupEnsure)
	}
	if _, ok := h.docker.containers["pmc-worker-1-org"]; !ok {
		t.Fatalf("no container named after this node: %v", h.docker.containers)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1"})
	if _, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.github.tokens) != 1 {
		t.Fatalf("two passes minted %d registration tokens, want 1 (a running runner is not re-registered)", len(h.github.tokens))
	}
	if len(h.docker.runCalls) != 1 {
		t.Fatalf("two passes ran %d containers, want 1", len(h.docker.runCalls))
	}
}

func TestUnselectedNodeRunsNothingAndRemovesItsLeftover(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-2"})
	// A leftover from an earlier selection.
	h.docker.containers["pmc-worker-1-org"] = Container{ID: "c1", Name: "pmc-worker-1-org", State: "running"}
	id := h.github.addRunner("acme", "pmc-worker-1-org", "online")

	if _, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.docker.runCalls) != 0 {
		t.Fatalf("an unselected node started a runner: %v", h.docker.runCalls)
	}
	if _, ok := h.docker.containers["pmc-worker-1-org"]; ok {
		t.Fatal("the unselected node kept its container")
	}
	if len(h.github.deleted) != 1 || h.github.deleted[0] != fmt.Sprintf("acme#%d", id) {
		t.Fatalf("de-registered = %v, want exactly the leftover", h.github.deleted)
	}
}

func TestManagerInTheListIsRefused(t *testing.T) {
	// The manager's OWN daemon is the safety net: it is the node that would run
	// the container, so it is the node that refuses.
	h := newHarness(t, harnessOpts{node: "manager-1", enabled: true, org: "acme", nodes: "manager-1", managerN: true})
	_, err := h.mgr.Reconcile(h.ctx)
	if !errors.Is(err, ErrManagerNode) {
		t.Fatalf("err = %v, want ErrManagerNode", err)
	}
	if !strings.Contains(err.Error(), "Down") {
		t.Fatalf("err = %q, want the evidence (a build on the manager marks the node Down)", err)
	}
	if len(h.docker.runCalls) != 0 {
		t.Fatal("a runner was started on a manager node")
	}
}

func TestEnableWithoutAppOrgOrNodesIsRefused(t *testing.T) {
	t.Run("no app", func(t *testing.T) {
		h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1"})
		h.mgr.GitHub = nil
		_, err := h.mgr.Reconcile(h.ctx)
		if !errors.Is(err, ErrNoApp) {
			t.Fatalf("err = %v, want ErrNoApp", err)
		}
		if !strings.Contains(err.Error(), "administration") {
			t.Fatalf("err = %q, want it to name the App permission", err)
		}
	})
	t.Run("no org", func(t *testing.T) {
		h := newHarness(t, harnessOpts{enabled: true, nodes: "worker-1"})
		_, err := h.mgr.Reconcile(h.ctx)
		if !errors.Is(err, ErrNoOrg) {
			t.Fatalf("err = %v, want ErrNoOrg", err)
		}
	})
	t.Run("no nodes", func(t *testing.T) {
		h := newHarness(t, harnessOpts{enabled: true, org: "acme"})
		_, err := h.mgr.Reconcile(h.ctx)
		if !errors.Is(err, ErrNoNodes) {
			t.Fatalf("err = %v, want ErrNoNodes", err)
		}
		if len(h.docker.runCalls) != 0 {
			t.Fatal("a runner started with no node selected")
		}
	})
}

func TestGroupVisibleToTheWholeOrgIsRefused(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1"})
	h.github.groupVis = githubapp.GroupVisibilityAll
	_, err := h.mgr.Reconcile(h.ctx)
	if err == nil {
		t.Fatal("a visibility=all group was accepted")
	}
	for _, want := range []string{"docker", "selected", "attacker"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to explain %q", err, want)
		}
	}
	if len(h.docker.runCalls) != 0 {
		t.Fatal("a runner started in an all-visible group")
	}
}

func TestDisableRemovesTheContainerAndDeregistersThenGoesQuiet(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1"})
	if _, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	id := h.github.addRunner("acme", "pmc-worker-1-org", "online")
	if err := h.store.SetSetting(h.ctx, cluster.SettingGitHubRunnersEnabled(), "false"); err != nil {
		t.Fatal(err)
	}
	res, err := h.mgr.Reconcile(h.ctx)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %v", res.Warnings)
	}
	if len(h.docker.removed) != 1 || h.docker.removed[0] != "pmc-worker-1-org" {
		t.Fatalf("removed containers = %v, want the runner", h.docker.removed)
	}
	if len(h.github.deleted) != 1 || h.github.deleted[0] != fmt.Sprintf("acme#%d", id) {
		t.Fatalf("de-registered = %v, want one call", h.github.deleted)
	}
	// The row is gone: the panel must not keep a ghost after the feature is off.
	if rows, _ := h.store.ListGitHubRunners(h.ctx, 200); len(rows) != 0 {
		t.Fatalf("rows after disable = %+v, want none", rows)
	}
	// A second disable is a clean no-op: nothing left, no API calls.
	before := len(h.github.deleted) + len(h.docker.removed) + len(h.github.tokens)
	if _, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	after := len(h.github.deleted) + len(h.docker.removed) + len(h.github.tokens)
	if after != before {
		t.Fatalf("a converged disabled pass made %d extra calls", after-before)
	}
}

// GitHub down must not keep the container alive: the docker access is local, and
// the report has to say the registration is unconfirmed rather than claim success.
func TestDisableWithGitHubDownStillRemovesTheContainer(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1"})
	if _, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.github.listErr = errors.New("dial tcp: connection refused")
	if err := h.store.SetSetting(h.ctx, cluster.SettingGitHubRunnersEnabled(), "false"); err != nil {
		t.Fatal(err)
	}
	res, err := h.mgr.Reconcile(h.ctx)
	if err != nil {
		t.Fatalf("a GitHub outage must not fail the disable: %v", err)
	}
	if len(h.docker.removed) != 1 {
		t.Fatalf("the container was not removed while GitHub was down: %v", h.docker.removed)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "de-register") {
		t.Fatalf("warnings = %v, want one naming the unconfirmed de-registration", res.Warnings)
	}
	// The bookkeeping entry survives so a later pass retries the de-registration.
	if st := loadState(h.ctx, h.store); len(st.Runners) != 1 {
		t.Fatalf("state = %+v, want the unconfirmed entry kept for a retry", st.Runners)
	}
}

func TestRegistrationFailureIsAWarningAndRecordsTheError(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1"})
	h.github.tokenErr = fmt.Errorf("%w: no administration:write", githubapp.ErrInsufficientPermission)
	res, err := h.mgr.Reconcile(h.ctx)
	if err != nil {
		t.Fatalf("a failed registration must not fail the pass: %v", err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "administration") {
		t.Fatalf("warnings = %v, want one naming the permission", res.Warnings)
	}
	if st := loadState(h.ctx, h.store); st.Runners["pmc-worker-1-org"].Failures != 1 || st.Runners["pmc-worker-1-org"].LastError == "" {
		t.Fatalf("state = %+v, want the failure recorded for the doctor", st.Runners)
	}
}

// ---- console view ----------------------------------------------------------

func TestRunnersViewMergesGitHubStateWithTheSelection(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1,worker-2"})
	h.github.addRunner("acme", "pmc-worker-1-org", "online")
	rows, err := h.mgr.Runners(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want one per selected node", rows)
	}
	if rows[0].Name != "pmc-worker-1-org" || rows[0].Org != "acme" || rows[0].Node != "worker-1" {
		t.Fatalf("row[0] = %+v", rows[0])
	}
	if rows[0].State != StateIdle {
		t.Fatalf("state = %q, want idle (registered, not busy)", rows[0].State)
	}
	if rows[1].State != StateOffline {
		t.Fatalf("row[1] state = %q, want offline (not registered)", rows[1].State)
	}
}

func TestDisabledFeatureHasNoRows(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: false, org: "acme", nodes: "worker-1"})
	if _, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := h.mgr.Runners(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %+v, want none while the feature is off", rows)
	}
}

// A node that is not selected must not de-register ANOTHER node's runner. The
// bookkeeping is keyed by runner name for exactly this reason: a store adopted by
// several daemons would otherwise turn one node leaving the list into a
// cluster-wide sweep.
func TestOneNodeNeverDeregistersAnotherNodesRunner(t *testing.T) {
	h := newHarness(t, harnessOpts{node: "worker-1", enabled: true, org: "acme", nodes: "worker-2"})
	other := RunnerName("worker-2")
	st := loadState(h.ctx, h.store)
	st.Runners[other] = RunnerState{Name: other, Org: "acme", RegisteredAt: 1}
	if err := saveState(h.ctx, h.store, st); err != nil {
		t.Fatal(err)
	}
	id := h.github.addRunner("acme", other, "online")

	if _, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.github.deleted) != 0 {
		t.Fatalf("node worker-1 de-registered %v — another node's runner is that node's to stop", h.github.deleted)
	}
	if len(h.github.runners["acme"]) != 1 || h.github.runners["acme"][0].ID != id {
		t.Fatal("another node's registration was removed")
	}
	if _, ok := loadState(h.ctx, h.store).Runners[other]; !ok {
		t.Fatal("another node's bookkeeping entry was dropped")
	}
}

// The node that OWNS the entry does tear it down when it leaves the list.
func TestSelectedNodeLeavingTheListTearsDownItsOwnRunner(t *testing.T) {
	h := newHarness(t, harnessOpts{node: "worker-1", enabled: true, org: "acme", nodes: "worker-1"})
	if _, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	id := h.github.addRunner("acme", RunnerName("worker-1"), "online")
	if err := h.store.SetSetting(h.ctx, cluster.SettingGitHubRunnerNodes(), "worker-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.docker.removed) != 1 || h.docker.removed[0] != RunnerName("worker-1") {
		t.Fatalf("removed = %v, want this node's container", h.docker.removed)
	}
	if len(h.github.deleted) != 1 || h.github.deleted[0] != "acme#"+itoa(id) {
		t.Fatalf("deleted = %v, want this node's registration", h.github.deleted)
	}
}

// TestArgvDeclaresTheRunnerScope: the image validates ORG_NAME only when
// RUNNER_SCOPE says "org". Without it a live runner exited 1 with
// "Invalid configuration provided for url" and the reconcile restarted it —
// a crash loop whose only symptom was a container that never stayed up.
func TestArgvDeclaresTheRunnerScope(t *testing.T) {
	s := Spec{Org: "acme", Token: "tok", RunnerName: "pmc-node-org", SocketProxy: true}
	args, err := s.Argv()
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "RUNNER_SCOPE=org") {
		t.Fatalf("args lack RUNNER_SCOPE=org, so the image treats this as a repo runner: %v", args)
	}
	if strings.Contains(joined, "latest") {
		t.Fatalf("the runner image is not pinned: %v", args)
	}
}

// TestRestartOnAnotherNodeClearsTheHoldAndNamesTheOwner is the regression for a
// restart that behaved like a failure: the leader cannot recreate a worker's
// container, so the old code fell through to its own selection check and answered
// "node <leader> is not in github_runner_nodes" — naming the wrong machine, after
// having already deregistered the runner.
func TestRestartOnAnotherNodeClearsTheHoldAndNamesTheOwner(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1"})
	// The manager here runs on worker-2: a different node from the runner's.
	h.mgr.Node = "worker-2"

	out, err := h.mgr.Restart(h.ctx, RunnerName("worker-1"), false)
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if out.RegistrationError != "" {
		t.Fatalf("a restart of another node's runner reported an error: %q", out.RegistrationError)
	}
	if !strings.Contains(out.Registration, "worker-1") {
		t.Fatalf("the answer does not name the owning node: %q", out.Registration)
	}
	if strings.Contains(out.Registration, "worker-2") {
		t.Fatalf("the answer names THIS node instead of the runner's: %q", out.Registration)
	}
	// The hold is what the console exists to clear; it must not survive.
	if h.mgr.HeldName(h.ctx, RunnerName("worker-1")) {
		t.Fatal("the hold survived the restart")
	}
}

// TestEphemeralRunnerBetweenJobsIsNotOffline: an ephemeral runner exits 0 when its
// job finishes and the reconcile starts another. Between those two moments there is
// no container — which the state machine can only call "offline", the word an
// operator reads as "my CI is broken". Measured on the live fleet: a healthy worker
// showed offline for the minute between two builds.
func TestEphemeralRunnerBetweenJobsIsNotOffline(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1"})
	h.mgr.Node = "worker-2" // the row belongs to another node: no container visible

	// It has run before — RegisteredAt is set when a container last started with a
	// fresh token — which is what makes "between jobs" true rather than "never
	// started".
	st := loadState(h.ctx, h.mgr.Store)
	st.Runners[RunnerName("worker-1")] = RunnerState{
		Name: RunnerName("worker-1"), Org: "acme",
		RegisteredAt: h.mgr.now().Add(-10 * time.Minute).Unix(),
	}
	if err := saveState(h.ctx, h.mgr.Store, st); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	rows, err := h.mgr.Runners(h.ctx)
	if err != nil {
		t.Fatalf("Runners: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].State != StateBetweenJobs {
		t.Fatalf("state = %q, want %q (a selected ephemeral runner with no container is between jobs)", rows[0].State, StateBetweenJobs)
	}
}

// …but a runner that keeps failing is NOT "between jobs": deriveRunnerState has
// already said failed, and that must survive.
func TestAFailingRunnerIsNotCalledBetweenJobs(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1"})
	h.mgr.Node = "worker-2"
	st := loadState(h.ctx, h.mgr.Store)
	st.Runners[RunnerName("worker-1")] = RunnerState{Failures: 4, LastError: "boom"}
	if err := saveState(h.ctx, h.mgr.Store, st); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	rows, err := h.mgr.Runners(h.ctx)
	if err != nil {
		t.Fatalf("Runners: %v", err)
	}
	if rows[0].State != StateFailed {
		t.Fatalf("state = %q, want %q", rows[0].State, StateFailed)
	}
}
