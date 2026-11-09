package runners

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// ---- render -----------------------------------------------------------------

// renderedSpec is the shape the tests assert against: the same compose doc the
// deployer sends to `docker stack deploy`.
type renderedSpec struct {
	Services map[string]struct {
		Image       string            `json:"image"`
		Environment map[string]string `json:"environment"`
		Volumes     []struct {
			Type   string `json:"type"`
			Source string `json:"source"`
			Target string `json:"target"`
		} `json:"volumes"`
		Labels map[string]string `json:"labels"`
		Deploy struct {
			Replicas      int `json:"replicas"`
			RestartPolicy struct {
				Condition string `json:"condition"`
			} `json:"restart_policy"`
			Placement struct {
				Constraints []string `json:"constraints"`
			} `json:"placement"`
			Resources struct {
				Limits struct {
					Memory string `json:"memory"`
				} `json:"limits"`
			} `json:"resources"`
			UpdateConfig *struct {
				Order string `json:"order"`
			} `json:"update_config"`
		} `json:"deploy"`
	} `json:"services"`
}

func renderForTest(t *testing.T, spec Spec) renderedSpec {
	t.Helper()
	body, err := Render(spec)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var doc renderedSpec
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal rendered compose: %v\n%s", err, body)
	}
	return doc
}

func TestRenderExcludesTheLeaderAndMountsTheDockerSocket(t *testing.T) {
	doc := renderForTest(t, Spec{
		Repo:       "acme/api",
		RunnerName: "pmc-node1-acme-api",
		Token:      "reg-token",
		Memory:     "2560m",
	})
	svc, ok := doc.Services["runner"]
	if !ok {
		t.Fatalf("rendered compose has no runner service: %+v", doc.Services)
	}

	// The placement claim must be in the SPEC, not a convention: Swarm
	// enforces constraints, a runbook does not.
	const want = "node.labels.pmcluster.leader != true"
	found := false
	for _, c := range svc.Deploy.Placement.Constraints {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("placement constraints = %v, want to include %q", svc.Deploy.Placement.Constraints, want)
	}

	// Root-equivalent mount, documented and deliberate.
	if len(svc.Volumes) != 1 || svc.Volumes[0].Source != "/var/run/docker.sock" || svc.Volumes[0].Target != "/var/run/docker.sock" {
		t.Fatalf("volumes = %+v, want the docker socket bound to itself", svc.Volumes)
	}

	// One job then exit.
	if svc.Environment["EPHEMERAL"] != "true" {
		t.Errorf("EPHEMERAL = %q, want true", svc.Environment["EPHEMERAL"])
	}
	if svc.Deploy.RestartPolicy.Condition != "none" {
		t.Errorf("restart condition = %q, want none (an ephemeral runner must not be restarted onto a spent registration token)", svc.Deploy.RestartPolicy.Condition)
	}
	if svc.Deploy.Replicas != 1 {
		t.Errorf("replicas = %d, want 1", svc.Deploy.Replicas)
	}
	if got := svc.Environment["LABELS"]; got != Labels {
		t.Errorf("LABELS = %q, want %q", got, Labels)
	}
	if got := svc.Environment["REPO_URL"]; got != "https://github.com/acme/api" {
		t.Errorf("REPO_URL = %q", got)
	}
	if svc.Labels[LabelRunner] != "true" || svc.Labels[LabelRepo] != "acme/api" {
		t.Errorf("service labels = %v, want the runner/repo discovery labels", svc.Labels)
	}
	// Two tasks must never register the same runner name at once.
	if svc.Deploy.UpdateConfig == nil || svc.Deploy.UpdateConfig.Order != "stop-first" {
		t.Errorf("update_config = %+v, want stop-first", svc.Deploy.UpdateConfig)
	}
}

func TestRenderAllowLeaderLiftsTheConstraint(t *testing.T) {
	doc := renderForTest(t, Spec{Repo: "acme/api", Token: "t", AllowLeader: true})
	if got := doc.Services["runner"].Deploy.Placement.Constraints; len(got) != 0 {
		t.Fatalf("constraints = %v, want none when the operator allowed the leader", got)
	}
}

func TestMemorySettingReachesTheRenderedContainer(t *testing.T) {
	for _, mem := range []string{"2560m", "4g"} {
		doc := renderForTest(t, Spec{Repo: "acme/api", Token: "t", Memory: mem})
		if got := doc.Services["runner"].Deploy.Resources.Limits.Memory; got != mem {
			t.Errorf("memory limit = %q, want %q (the setting must reach the container verbatim)", got, mem)
		}
	}
}

func TestRunnerNameIsStableAndPrefixed(t *testing.T) {
	name := RunnerName("nxt-sw-1-m", "acme/api")
	if !strings.HasPrefix(name, "pmc-nxt-sw-1-m-") {
		t.Fatalf("runner name = %q, want pmc-<node>-<repo-short>", name)
	}
	if name != RunnerName("nxt-sw-1-m", "acme/api") {
		t.Fatal("runner name is not deterministic")
	}
	if len(name) > 64 {
		t.Fatalf("runner name %q is longer than GitHub's 64-character limit", name)
	}
	if StackName("acme/api") == StackName("acme-frontend/api") {
		t.Fatal("two different repos collapsed to the same stack name")
	}
}

func TestParseReposTrimsAndDedupes(t *testing.T) {
	got := ParseRepos(" acme/api, acme/web ,acme/api,,")
	want := []string{"acme/api", "acme/web"}
	if len(got) != len(want) {
		t.Fatalf("ParseRepos = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ParseRepos = %v, want %v", got, want)
		}
	}
	if _, err := NormalizeRepos("acme"); err == nil {
		t.Fatal("NormalizeRepos accepted a bare name with no owner")
	}
	if _, err := NormalizeRepos("acme/api/extra"); err == nil {
		t.Fatal("NormalizeRepos accepted a path with three segments")
	}
}

func TestValidateMemory(t *testing.T) {
	for _, ok := range []string{"2560m", "3g", "512M", "1073741824"} {
		if err := ValidateMemory(ok); err != nil {
			t.Errorf("ValidateMemory(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "lots", "0", "-1g", "256 m"} {
		if err := ValidateMemory(bad); err == nil {
			t.Errorf("ValidateMemory(%q) = nil, want an error", bad)
		}
	}
}

// ---- lifecycle fakes --------------------------------------------------------

// fakeDeployer turns a rendered compose into a live service entry, so the next
// pass sees what a real Swarm would: one running task.
type fakeDeployer struct {
	services map[string]runtime.Service
	deploys  []string
	removed  []string
	failOn   map[string]error
}

func newFakeDeployer() *fakeDeployer {
	return &fakeDeployer{services: map[string]runtime.Service{}, failOn: map[string]error{}}
}

func (f *fakeDeployer) DeployStackNoPrune(_ context.Context, name string, composeYAML []byte) error {
	if err := f.failOn[name]; err != nil {
		return err
	}
	var doc renderedSpec
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return fmt.Errorf("fake deployer: %w", err)
	}
	svc := doc.Services["runner"]
	f.deploys = append(f.deploys, name)
	f.services[name] = runtime.Service{
		Name:     name + "_runner",
		Stack:    name,
		Replicas: 1,
		Desired:  1,
		Labels:   svc.Labels,
	}
	return nil
}

func (f *fakeDeployer) RemoveStack(_ context.Context, name string) error {
	delete(f.services, name)
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeDeployer) ServiceList(context.Context) ([]runtime.Service, error) {
	out := make([]runtime.Service, 0, len(f.services))
	for _, s := range f.services {
		out = append(out, s)
	}
	return out, nil
}

// fakeGitHub is the App surface, in memory. It records every call so the tests
// can count registrations and de-registrations instead of trusting prose.
type fakeGitHub struct {
	tokens     map[string]string
	tokenErr   map[string]error
	private    map[string]bool
	runners    map[string][]githubapp.Runner
	perms      map[string]string
	tokenCalls []string
	listCalls  []string
	deleted    []string // repo#id
	nextRunner int64
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		tokens:   map[string]string{},
		tokenErr: map[string]error{},
		private:  map[string]bool{},
		runners:  map[string][]githubapp.Runner{},
		perms:    map[string]string{"administration": "write", "contents": "read"},
	}
}

func (f *fakeGitHub) RunnerRegistrationToken(_ context.Context, repo string) (string, error) {
	f.tokenCalls = append(f.tokenCalls, repo)
	if err := f.tokenErr[repo]; err != nil {
		return "", err
	}
	return "token-" + repo, nil
}

func (f *fakeGitHub) ListRunners(_ context.Context, repo string) ([]githubapp.Runner, error) {
	f.listCalls = append(f.listCalls, repo)
	return f.runners[repo], nil
}

func (f *fakeGitHub) DeleteRunner(_ context.Context, repo string, id int64) error {
	f.deleted = append(f.deleted, fmt.Sprintf("%s#%d", repo, id))
	for i, r := range f.runners[repo] {
		if r.ID == id {
			f.runners[repo] = append(f.runners[repo][:i], f.runners[repo][i+1:]...)
			break
		}
	}
	return nil
}

func (f *fakeGitHub) RepoPrivate(_ context.Context, repo string) (bool, error) {
	if p, ok := f.private[repo]; ok {
		return p, nil
	}
	return true, nil
}

func (f *fakeGitHub) GrantedPermissions(context.Context) (map[string]string, error) {
	return f.perms, nil
}

// addRunner seeds a registration as if a container had registered itself.
func (f *fakeGitHub) addRunner(repo, name, status string) {
	f.nextRunner++
	f.runners[repo] = append(f.runners[repo], githubapp.Runner{ID: f.nextRunner, Name: name, Status: status})
}

// ---- lifecycle tests --------------------------------------------------------

type harness struct {
	store    *store.Store
	github   *fakeGitHub
	deployer *fakeDeployer
	mgr      *Manager
	ctx      context.Context
}

func newHarness(t *testing.T, enabled bool, repos, memory string) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for k, v := range map[string]string{
		cluster.SettingGitHubRunnersEnabled(): fmt.Sprintf("%t", enabled),
		cluster.SettingGitHubRunnerRepos():    repos,
		cluster.SettingGitHubRunnerMemory():   memory,
	} {
		if err := st.SetSetting(ctx, k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	gh := newFakeGitHub()
	dep := newFakeDeployer()
	return &harness{
		store:    st,
		github:   gh,
		deployer: dep,
		mgr: &Manager{
			Store:    st,
			GitHub:   gh,
			Deployer: dep,
			Cluster:  dep,
			Node:     "node1",
		},
		ctx: ctx,
	}
}

func TestEnableRegistersExactlyOneRunnerPerRepo(t *testing.T) {
	h := newHarness(t, true, "acme/api,acme/web", "2560m")

	res, err := h.mgr.Reconcile(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %v", res.Warnings)
	}
	if len(res.Registered) != 2 {
		t.Fatalf("registered = %v, want two repos", res.Registered)
	}
	if len(h.deployer.services) != 2 {
		t.Fatalf("deployed %d stacks, want 2", len(h.deployer.services))
	}
	// The stable name/labels each repo's container was created with.
	for repo, stack := range map[string]string{"acme/api": StackName("acme/api"), "acme/web": StackName("acme/web")} {
		svc, ok := h.deployer.services[stack]
		if !ok {
			t.Fatalf("no deployed stack %s for %s", stack, repo)
		}
		if svc.Labels[LabelRepo] != repo {
			t.Errorf("%s carries repo label %q", stack, svc.Labels[LabelRepo])
		}
		if svc.Labels[LabelName] != RunnerName("node1", repo) {
			t.Errorf("%s carries runner name %q, want %q", stack, svc.Labels[LabelName], RunnerName("node1", repo))
		}
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	h := newHarness(t, true, "acme/api", "2560m")
	if _, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.github.tokenCalls) != 1 {
		t.Fatalf("first pass minted %d registration tokens, want 1", len(h.github.tokenCalls))
	}
	if _, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.github.tokenCalls) != 1 {
		t.Fatalf("second pass minted %d registration tokens total, want still 1 (a running runner is not re-registered)", len(h.github.tokenCalls))
	}
	if len(h.deployer.deploys) != 1 {
		t.Fatalf("deployed %d times, want 1 (the second pass must not touch a running container)", len(h.deployer.deploys))
	}
}

func TestRemovedRepoIsDeregisteredAndItsContainerRemoved(t *testing.T) {
	h := newHarness(t, true, "acme/api,acme/web", "2560m")
	if _, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"acme/api", "acme/web"} {
		h.github.addRunner(repo, RunnerName("node1", repo), "online")
	}
	// Take acme/web out of the list.
	if err := h.store.SetSetting(h.ctx, cluster.SettingGitHubRunnerRepos(), "acme/api"); err != nil {
		t.Fatal(err)
	}
	res, err := h.mgr.Reconcile(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != "acme/web" {
		t.Fatalf("removed = %v, want [acme/web]", res.Removed)
	}
	if len(h.deployer.removed) != 1 || h.deployer.removed[0] != StackName("acme/web") {
		t.Fatalf("removed stacks = %v, want only acme/web's", h.deployer.removed)
	}
	if len(h.github.deleted) != 1 || !strings.HasPrefix(h.github.deleted[0], "acme/web#") {
		t.Fatalf("de-registered = %v, want the acme/web runner only", h.github.deleted)
	}
	// The remaining repo is untouched.
	if _, ok := h.deployer.services[StackName("acme/api")]; !ok {
		t.Fatal("removing one repo removed the other repo's runner")
	}
}

func TestDisableRemovesEveryContainerAndDeregistersEveryRunner(t *testing.T) {
	h := newHarness(t, true, "acme/api,acme/web", "2560m")
	if _, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"acme/api", "acme/web"} {
		h.github.addRunner(repo, RunnerName("node1", repo), "online")
	}
	if err := h.store.SetSetting(h.ctx, cluster.SettingGitHubRunnersEnabled(), "false"); err != nil {
		t.Fatal(err)
	}
	res, err := h.mgr.Reconcile(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %v", res.Warnings)
	}
	if len(h.deployer.removed) != 2 {
		t.Fatalf("removed %d containers, want 2 (%v)", len(h.deployer.removed), h.deployer.removed)
	}
	if len(h.deployer.services) != 0 {
		t.Fatalf("%d service(s) survived the disable", len(h.deployer.services))
	}
	if len(h.github.deleted) != 2 {
		t.Fatalf("DeleteRunner called %d times, want 2 (%v)", len(h.github.deleted), h.github.deleted)
	}
	// A further pass is a no-op: nothing left to remove, no API calls.
	before := len(h.github.listCalls) + len(h.github.deleted)
	if _, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if after := len(h.github.listCalls) + len(h.github.deleted); after != before {
		t.Fatalf("a converged disabled pass made %d extra API calls", after-before)
	}
}

func TestRegistrationFailureIsAWarningAndDoesNotAbortTheRest(t *testing.T) {
	h := newHarness(t, true, "acme/api,acme/web", "2560m")
	h.github.tokenErr["acme/api"] = fmt.Errorf("%w: no administration:write", githubapp.ErrInsufficientPermission)

	res, err := h.mgr.Reconcile(h.ctx)
	if err != nil {
		t.Fatalf("a per-repo failure must not fail the pass: %v", err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "acme/api") {
		t.Fatalf("warnings = %v, want one naming acme/api", res.Warnings)
	}
	if len(res.Registered) != 1 || res.Registered[0] != "acme/web" {
		t.Fatalf("registered = %v, want acme/web (the healthy repo)", res.Registered)
	}
	// The failure is persisted for the doctor, with a consecutive count.
	st := loadState(h.ctx, h.store)
	if got := st.Repos["acme/api"].Failures; got != 1 {
		t.Fatalf("failure count = %d, want 1", got)
	}
	if st.Repos["acme/api"].LastError == "" {
		t.Fatal("last error was not recorded")
	}
}

func TestPublicRepoIsRefused(t *testing.T) {
	h := newHarness(t, true, "acme/public", "")
	h.github.private["acme/public"] = false

	res, err := h.mgr.Reconcile(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.github.tokenCalls) != 0 {
		t.Fatal("a public repository was registered — a fork PR would run on the docker socket")
	}
	if len(h.deployer.deploys) != 0 {
		t.Fatal("a container was deployed for a public repository")
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "public") {
		t.Fatalf("warnings = %v, want one naming the public-repo refusal", res.Warnings)
	}
}

func TestGhostRunnerIsDeregisteredAfterItsContainerDisappears(t *testing.T) {
	h := newHarness(t, true, "acme/api", "2560m")
	if _, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.github.addRunner("acme/api", RunnerName("node1", "acme/api"), "online")

	// The container is gone AND the repo left the list: the bookkeeping is the
	// only thing that remembers which runner name to de-register.
	delete(h.deployer.services, StackName("acme/api"))
	if err := h.store.SetSetting(h.ctx, cluster.SettingGitHubRunnerRepos(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.mgr.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.github.deleted) != 1 {
		t.Fatalf("de-registered %d ghost runner(s), want 1 (%v)", len(h.github.deleted), h.github.deleted)
	}
}

func TestInspectReportsPermissionOfflineAndGhosts(t *testing.T) {
	h := newHarness(t, true, "acme/api", "2560m")
	h.github.perms = map[string]string{"contents": "read"}
	h.github.addRunner("acme/api", RunnerName("node1", "acme/api"), "offline")

	rep, err := h.mgr.Inspect(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Enabled || !rep.AppConfigured {
		t.Fatalf("report = %+v, want enabled with an app", rep)
	}
	if rep.MissingPermission != RequiredPermission {
		t.Fatalf("missing permission = %q, want %q", rep.MissingPermission, RequiredPermission)
	}
	if len(rep.Repos) != 1 {
		t.Fatalf("repos = %+v", rep.Repos)
	}
	rr := rep.Repos[0]
	if !rr.Registered {
		t.Error("runner was not reported as registered")
	}
	if len(rr.Offline) != 1 {
		t.Errorf("offline = %v, want the registered-but-offline runner", rr.Offline)
	}
	if rr.ContainerRunning {
		t.Error("reported a running container for a repo nothing deployed")
	}
}

func TestInspectDisabledMakesNoCalls(t *testing.T) {
	h := newHarness(t, false, "acme/api", "")
	rep, err := h.mgr.Inspect(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Enabled {
		t.Fatal("disabled feature reported as enabled")
	}
	if len(h.github.listCalls) != 0 {
		t.Fatalf("inspect on a disabled feature called GitHub %d time(s)", len(h.github.listCalls))
	}
}
