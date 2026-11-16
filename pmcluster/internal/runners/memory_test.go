package runners

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodestat"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// TestCheckMemoryFit_TheIncident: the two nodes from 2026-11-10. This is the
// rule's reason for existing, so it is the first test.
func TestCheckMemoryFit_TheIncident(t *testing.T) {
	// nxt-sw-3-w: 1839 MB of RAM, granted a 2560m cap — 139 % of the host.
	err := CheckMemoryFit("nxt-sw-3-w", 1839, true, "2560m")
	if err == nil {
		t.Fatal("2560m on a 1839 MB node was allowed; that is the assignment that OOM-killed the host")
	}
	if !errors.Is(err, ErrRunnerMemory) {
		t.Errorf("refusal does not wrap ErrRunnerMemory: %v", err)
	}
	var ref *MemoryRefusal
	if !errors.As(err, &ref) {
		t.Fatalf("refusal is not a *MemoryRefusal: %v", err)
	}
	// BOTH numbers, and the node, or the message cannot be acted on.
	for _, want := range []string{"nxt-sw-3-w", "1839", "2560m", "lower github_runner_memory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal text is missing %q: %s", want, err)
		}
	}
	if ref.HostMiB != 1839 || ref.CapMiB != 2560 {
		t.Errorf("refusal reports %d MB host / %d MiB cap, want 1839 / 2560", ref.HostMiB, ref.CapMiB)
	}

	// The same cap on a 3852 MB node is the platform's documented default and
	// must keep working.
	if err := CheckMemoryFit("nxt-sw-6-w", 3852, true, "2560m"); err != nil {
		t.Fatalf("2560m on a 3852 MB node was refused: %v", err)
	}
}

func TestCheckMemoryFit_UnknownRAMAllows(t *testing.T) {
	for _, tc := range []struct {
		host  int64
		known bool
	}{
		{0, false},    // no sample at all
		{0, true},     // a sample with no MemTotal
		{1839, false}, // a sample we cannot trust
	} {
		if err := CheckMemoryFit("nxt-sw-3-w", tc.host, tc.known, "2560m"); err != nil {
			t.Errorf("unknown RAM (%d MB, known=%t) refused a runner: %v — a monitoring gap must not stop CI",
				tc.host, tc.known, err)
		}
	}
}

// TestCheckMemoryFit_BoundaryIsExact: the share itself is allowed, one MiB over
// is not. Integer arithmetic, so there is no float rounding at the line.
func TestCheckMemoryFit_BoundaryIsExact(t *testing.T) {
	const host = int64(1000) // 70 % = exactly 700 MiB
	if err := CheckMemoryFit("n", host, true, "700m"); err != nil {
		t.Errorf("a cap exactly at the ceiling was refused: %v", err)
	}
	if err := CheckMemoryFit("n", host, true, "701m"); err == nil {
		t.Error("a cap 1 MiB over the ceiling was allowed")
	}
	if err := CheckMemoryFit("n", host, true, ""); err != nil {
		// An unset cap is the settings default's problem, not this rule's.
		t.Errorf("an empty cap was refused: %v", err)
	}
	if err := CheckMemoryFit("n", host, true, "not-a-quantity"); err != nil {
		t.Errorf("an unparseable cap was refused by the memory rule (ValidateMemory owns that): %v", err)
	}
}

func TestParseMemoryMiB(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"2560m", 2560},
		{"2560M", 2560},
		{" 2560m ", 2560},
		{"3g", 3072},
		{"1.5g", 1536},
		{"512k", 0}, // half a MiB, rounded down
		{"512", 0},  // Docker reads a bare number as bytes
		{"2048", 0}, // 2048 bytes is not 2048 MiB
		{"0m", 0},
	} {
		got, err := ParseMemoryMiB(tc.in)
		if err != nil {
			t.Errorf("ParseMemoryMiB(%q) errored: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseMemoryMiB(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "abc", "12x", "-3g"} {
		if _, err := ParseMemoryMiB(bad); err == nil {
			t.Errorf("ParseMemoryMiB(%q) = nil error, want a refusal", bad)
		}
	}
}

// TestNodeMemoryMiB_ReadsTheLeaderStore: a worker's RAM exists only in the
// leader's node-stats rows (the swarm node list carries none).
func TestNodeMemoryMiB_ReadsTheLeaderStore(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	now := time.Now()

	write := func(node string, ramMB int64, sampleTS int64) {
		t.Helper()
		if err := st.UpsertNodeStats(ctx, []nodestat.Stat{{
			Hostname: node, MemTotalMB: ramMB, SampleTS: sampleTS, DockerUp: 1,
		}}, now.Unix()); err != nil {
			t.Fatalf("upsert %s: %v", node, err)
		}
	}

	write("fresh-node", 1839, now.Unix())
	write("stale-node", 1839, now.Add(-2*time.Hour).Unix())
	write("no-ram-node", 0, now.Unix())

	if mb, ok := NodeMemoryMiB(ctx, st, "fresh-node", now); !ok || mb != 1839 {
		t.Errorf("fresh node = %d MB (known=%t), want 1839/true", mb, ok)
	}
	// Hostnames compare case-insensitively: Swarm lowercases, an operator's
	// github_runner_nodes list need not.
	if mb, ok := NodeMemoryMiB(ctx, st, "FRESH-NODE", now); !ok || mb != 1839 {
		t.Errorf("case-insensitive lookup = %d MB (known=%t), want 1839/true", mb, ok)
	}
	if _, ok := NodeMemoryMiB(ctx, st, "stale-node", now); ok {
		t.Error("a two-hour-old sample counted as known; a node that stopped reporting is not a node we trust")
	}
	if _, ok := NodeMemoryMiB(ctx, st, "no-ram-node", now); ok {
		t.Error("a sample with MemTotal 0 counted as known")
	}
	if _, ok := NodeMemoryMiB(ctx, st, "absent-node", now); ok {
		t.Error("a node with no row counted as known")
	}
	if _, ok := NodeMemoryMiB(ctx, nil, "fresh-node", now); ok {
		t.Error("a nil store returned known RAM")
	}
}

// memoryService builds an assignment resolver whose CONFIG is mutable, so a
// settings change between two resolves is the real thing and not a copy.
func memoryService(cfg *Config, gh GitHub, ram int64, known bool, logs *strings.Builder) *AssignmentService {
	log := zerolog.Nop()
	if logs != nil {
		log = zerolog.New(logs)
	}
	return &AssignmentService{
		Config: func(context.Context) Config { return *cfg },
		Nodes:  fakeNodes{nodes: []runtime.Node{{Hostname: "worker-1", Role: "worker"}}},
		GitHub: gh,
		NodeMemory: func(_ context.Context, _ string) (int64, bool) {
			return ram, known
		},
		Now: func() time.Time { return time.Unix(1700000000, 0) },
		Log: log,
	}
}

// TestResolveAssignment_RefusesAnOversizedRunner is the leader-side contract:
// the refusal arrives as a DISABLED assignment with a reason and NO token, and it
// is re-evaluated on the next pass — which is what makes it survive a settings
// change.
func TestResolveAssignment_RefusesAnOversizedRunner(t *testing.T) {
	cfg := Config{Enabled: true, Org: "acme", Group: "pmcluster", Nodes: []string{"worker-1"}, Memory: "2560m", IntervalSeconds: 60}
	gh := newFakeGitHub()
	svc := memoryService(&cfg, gh, 1839, true, nil)

	a, err := svc.resolve(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("resolve returned an error instead of a refusal: %v", err)
	}
	if a.Enabled {
		t.Fatal("an assignment with a 2560m cap on a 1839 MB node was enabled")
	}
	if !a.Refused {
		t.Error("the assignment is not marked Refused; the node and the console cannot tell this from \"not selected\"")
	}
	for _, want := range []string{"1839", "2560m"} {
		if !strings.Contains(a.Reason, want) {
			t.Errorf("refusal reason is missing %q: %s", want, a.Reason)
		}
	}
	if a.RegistrationToken != "" {
		t.Error("a refused assignment carries a registration token")
	}
	if len(gh.tokens) != 0 {
		t.Errorf("a registration token was minted for a runner that must not start: %v", gh.tokens)
	}

	// A smaller cap on the same node is accepted on the NEXT pass: the rule is
	// re-evaluated, not recorded once. ("Survives a settings change" cuts both
	// ways — a stale refusal would be just as wrong as a stale approval.)
	cfg.Memory = "1200m"
	a2, err := svc.resolve(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("resolve after lowering the cap: %v", err)
	}
	if !a2.Enabled || a2.Refused {
		t.Fatalf("1200m on 1839 MB was not accepted: enabled=%t refused=%t reason=%s", a2.Enabled, a2.Refused, a2.Reason)
	}
	if a2.RegistrationToken == "" {
		t.Error("an accepted assignment has no registration token")
	}
	if len(gh.tokens) != 1 {
		t.Errorf("tokens minted = %d, want 1 (only the accepted pass)", len(gh.tokens))
	}
}

// TestResolveAssignment_UnknownRAMAllowsAndLogsOnce: a node with no telemetry
// still runs CI; the gap is one log line, not a refusal repeated every poll.
func TestResolveAssignment_UnknownRAMAllowsAndLogsOnce(t *testing.T) {
	cfg := Config{Enabled: true, Org: "acme", Group: "pmcluster", Nodes: []string{"worker-1"}, Memory: "2560m", IntervalSeconds: 60}
	gh := newFakeGitHub()
	var logs strings.Builder
	svc := memoryService(&cfg, gh, 0, false, &logs)

	for i := 0; i < 4; i++ {
		a, err := svc.resolve(context.Background(), "worker-1")
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		if !a.Enabled || a.Refused {
			t.Fatalf("pass %d: unknown RAM disabled the runner (enabled=%t refused=%t)", i, a.Enabled, a.Refused)
		}
	}
	if got := strings.Count(logs.String(), "node RAM unknown"); got != 1 {
		t.Errorf("the unknown-RAM note appears %d times across four passes, want exactly 1:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "worker-1") {
		t.Errorf("the note does not name the node:\n%s", logs.String())
	}
}

// TestRunnersRowSaysWhyAnOversizedCapIsNotRunning: the console row is where an
// operator looks when CI "just does not run". State stays `offline` (that IS the
// container's truth) and Reason carries the arithmetic.
func TestRunnersRowSaysWhyAnOversizedCapIsNotRunning(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1,worker-2"})
	h.mgr.NodeMemory = func(_ context.Context, node string) (int64, bool) {
		switch node {
		case "worker-1":
			return 1839, true
		case "worker-2":
			return 3852, true
		}
		return 0, false
	}
	rows, err := h.mgr.Runners(h.ctx)
	if err != nil {
		t.Fatalf("Runners: %v", err)
	}
	byNode := map[string]string{}
	state := map[string]string{}
	for _, r := range rows {
		byNode[r.Node] = r.Reason
		state[r.Node] = r.State
	}
	if !strings.Contains(byNode["worker-1"], "1839") || !strings.Contains(byNode["worker-1"], "2560m") {
		t.Errorf("worker-1 row reason = %q, want both numbers", byNode["worker-1"])
	}
	if state["worker-1"] != StateOffline {
		t.Errorf("worker-1 state = %q, want %q", state["worker-1"], StateOffline)
	}
	if byNode["worker-2"] != "" {
		t.Errorf("worker-2 row carries a refusal reason (%q) for a cap that fits", byNode["worker-2"])
	}
}

// TestInspectReportsTheRefusalForDoctor: `pmcluster doctor` on the leader must
// name the node, its RAM and the cap — the same three facts the refusal carries.
func TestInspectReportsTheRefusalForDoctor(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1"})
	h.github.addRunner("acme", "pmc-worker-2-org", "online") // keep the GitHub path exercised
	h.mgr.NodeMemory = func(_ context.Context, node string) (int64, bool) { return 1839, true }

	rep, err := h.mgr.Inspect(h.ctx)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(rep.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(rep.Nodes))
	}
	n := rep.Nodes[0]
	if !n.Refused {
		t.Fatal("Inspect did not report the refusal")
	}
	if !strings.Contains(n.Reason, "1839") || !strings.Contains(n.Reason, "2560m") {
		t.Errorf("Inspect reason = %q, want both numbers", n.Reason)
	}
	if n.ContainerRunning {
		t.Error("a refused node reports a running container")
	}
}

// TestAgentRefusalIsRecordedAndLogged: on the node, the refusal is persisted (so
// the local doctor can report it without a store) and logged once per pass with
// the leader's reason.
func TestAgentRefusalIsRecordedAndLogged(t *testing.T) {
	dir := t.TempDir()
	var logs strings.Builder
	dk := newFakeDocker()
	a := &Agent{
		Node:      "nxt-sw-3-w",
		Docker:    dk,
		StatePath: filepath.Join(dir, "runner.json"),
		Log:       zerolog.New(&logs),
	}
	as := Assignment{
		Node: "nxt-sw-3-w", Enabled: false, Refused: true,
		Org: "acme", Group: "pmcluster", Memory: "2560m",
		Reason: "refused: nxt-sw-3-w has 1839 MB; a 2560m runner can OOM this node and take it down — lower github_runner_memory or pick a bigger node",
	}
	if _, err := a.Apply(context.Background(), as); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(dk.runCalls) != 0 {
		t.Fatalf("a refused assignment started a container: %v", dk.runCalls)
	}
	rec, ok := a.Record()
	if !ok {
		t.Fatal("no agent record written")
	}
	if !rec.Refused || rec.Memory != "2560m" || !strings.Contains(rec.RefusedReason, "1839") {
		t.Errorf("agent record = %+v, want refused=true, memory=2560m, reason naming 1839 MB", rec)
	}
	if !strings.Contains(logs.String(), "does not fit this node") {
		t.Errorf("the refusal was not logged:\n%s", logs.String())
	}
}

// TestReconcile_RefusesTheSameRule: on a node whose own store is current (the
// single-node cluster, or the leader with github_runner_allow_leader) there is no
// assignment hop, so the reconcile applies the rule itself — the same function,
// so the two paths cannot disagree.
func TestReconcile_RefusesTheSameRule(t *testing.T) {
	h := newHarness(t, harnessOpts{enabled: true, org: "acme", nodes: "worker-1", node: "worker-1"})
	h.mgr.NodeMemory = func(_ context.Context, _ string) (int64, bool) { return 1839, true }

	_, err := h.mgr.Reconcile(h.ctx)
	if err == nil {
		t.Fatal("Reconcile started an oversized runner on a 1839 MB node")
	}
	if !errors.Is(err, ErrRunnerMemory) {
		t.Errorf("err = %v, want it to wrap ErrRunnerMemory so the doctor can classify it", err)
	}
	if len(h.docker.runCalls) != 0 {
		t.Fatalf("a container was started despite the refusal: %v", h.docker.runCalls)
	}
	if len(h.github.tokens) != 0 {
		t.Errorf("a registration token was minted for a refused runner: %v", h.github.tokens)
	}
}
