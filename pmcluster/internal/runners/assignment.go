package runners

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth/swarmtls"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// The assignment channel: one read-only pull over the swarm's own mTLS, from a
// node to the leader.
//
// Why it exists: cluster settings live in Docker configs, and reading configs is
// a MANAGER-ONLY call. A worker therefore cannot answer "am I a runner node?" for
// itself — measured on the live fleet, `docker config inspect` on a worker fails
// with "this node is not a swarm manager". Before this channel, the whole runner
// feature was inert on the only nodes allowed to host a runner.
//
// The decision stays server-side: the leader resolves the SAME rules the
// reconcile applies (feature switch, org, node list, manager/leader refusal,
// `selected` group visibility) and the node only applies the result. A node
// cannot promote itself into the selection by asking.
//
// What this channel is not (see internal/auth/swarmtls for the auth rules): it
// carries no cluster settings, no secrets, no stack configs, no credentials of
// the platform's own, and it never writes. The worker stops and removes its
// container; de-registering a runner at GitHub is done by the leader, which is
// the only side holding the App installation. The one GitHub artifact in the
// response is a single-use registration token — an ephemeral runner cannot be
// created without one — minted fresh per response, valid one hour, and useless
// for anything except registering the one runner name it is bound to.

const (
	// AssignmentPort is the dedicated mTLS port the leader serves this endpoint
	// on, bound to the node's swarm data-path address. It is not the API port
	// (9090) and nothing else is mounted on it.
	AssignmentPort = 9092
	// AssignmentPath is the route. It keeps the /api/github/runners prefix so
	// the endpoint reads as part of the runner surface, but it is NOT served by
	// the Bearer /api router: it is served only by the mTLS listener.
	AssignmentPath = "/api/github/runners/assignment"

	// DefaultAssignmentIntervalSeconds is how often a node re-reads its
	// assignment when nothing overrides it. The leader sends the interval in
	// every response, because a worker cannot read the setting itself.
	DefaultAssignmentIntervalSeconds = 60

	// assignmentDeregisterInterval bounds how often the leader re-checks
	// GitHub for a registration to remove for a node it has disabled. Without
	// it, every disabled node's poll would list the org's runners once a
	// minute for nothing.
	assignmentDeregisterInterval = 5 * time.Minute
)

// Assignment is the leader's resolved answer to "what should THIS node run?".
// Every field is either a decision (enabled/organisation/group/memory/socket) or
// an identity/liveness detail; nothing here is a cluster setting dump.
type Assignment struct {
	Node    string `json:"node"`
	Enabled bool   `json:"enabled"`
	Org     string `json:"org"`
	Group   string `json:"group"`
	// NodeDockerRestart arms THIS node's watcher to restart docker when its socket
	// stops answering.
	//
	// It travels in the assignment because the setting lives on the leader and a
	// worker has no store to read it from — the first version read the local store,
	// which on a worker always returned the default, so the feature silently did
	// nothing on exactly the nodes that need it (measured on nxt-sw-6-w: the setting
	// said true, the watchdog unit never gained the flag).
	NodeDockerRestart bool `json:"node_docker_restart"`
	// It travels in this document because the decision is the operator's, made once
	// on the leader: a worker that chose for itself would make "what does a job
	// get?" depend on which machine picked up the job.
	Memory string `json:"memory"`
	Mode   string `json:"mode"`
	Labels string `json:"labels"`
	// IntervalSeconds is how long the node should wait before asking again. It
	// travels in the response because the setting it comes from is manager-only.
	IntervalSeconds int `json:"interval_seconds"`
	// Reason explains a disabled answer ("the feature is off", "this node is
	// not selected"). It is for the node's log, never for a decision.
	Reason string `json:"reason"`
	// Held is an OPERATOR STOP that must cross the node boundary. The console's
	// Kill runs on the leader, which can only remove a container on its own
	// node: a kill for a remote node was recorded here and nowhere else, so the
	// node restarted the runner seconds later — the button appeared to work and
	// changed nothing. Carrying the flag in the assignment makes the node act.
	Held bool `json:"held"`
	// Refused is a DISABLED answer with a reason that is an arithmetic refusal,
	// not a setting: this node cannot host a runner with the configured memory
	// cap. It is separate from Enabled=false so the node's log, its doctor and
	// the console row can say "refused" — which is an operator action — instead
	// of "not selected", which is not.
	Refused bool `json:"refused"`
	// RegistrationToken is the single-use, hour-lived org registration token the
	// container registers with. Empty when the assignment is disabled.
	RegistrationToken string `json:"registration_token"`
	// RegistrationTokenExpiresAt is when that token stops working (unix).
	RegistrationTokenExpiresAt int64 `json:"registration_token_expires_at"`
	// IssuedAt is when the leader resolved this answer (unix).
	IssuedAt int64 `json:"issued_at"`
}

// Config renders the assignment as the settings view the container spec uses.
// It is how the node applies the leader's decision with the same Spec builder and
// the same mount rules the leader uses for its own (refused) node.
func (a Assignment) Config(node string) Config {
	if node == "" {
		node = a.Node
	}
	return Config{
		Enabled: a.Enabled,
		Org:     a.Org,
		Group:   a.Group,
		Memory:  a.Memory,
		Node:    node,
		Image:   DefaultImage,
	}
}

// Interval returns the poll interval the node should use, clamped to something
// that neither hammers GitHub nor leaves an assignment stale for an hour.
func (a Assignment) Interval() time.Duration {
	secs := a.IntervalSeconds
	if secs <= 0 {
		secs = DefaultAssignmentIntervalSeconds
	}
	if secs < 10 {
		secs = 10
	}
	if secs > 3600 {
		secs = 3600
	}
	return time.Duration(secs) * time.Second
}

// isManagerNode is the one place that decides what "a manager" means for placement. It
// is deliberately generous about the leader flag: a node the swarm reports as leader IS a
// manager, and a name list that says manager without the role flag is one too.
func isManagerNode(n runtime.Node) bool {
	return n.IsLeader || strings.EqualFold(n.Role, "manager")
}

// CheckPlacement refuses a selection that contains a manager, unless the operator
// lifted the rule for the leader with `github_runner_allow_leader`.
//
// One function, used by both the reconcile (Manager.checkNodeRole, for the leader's own
// view) and the assignment resolver, so "exactly as the reconcile does" stays literally
// true. That mattered: the switch was read into Config, plumbed all the way here, and
// then ignored — the console offered it, the daemon never consulted it, and an operator
// could tick it forever without a runner appearing.
//
// Why the leader is still a bad default: it serves the API and the control loop, so a
// build competes for its CPU and memory. Why it is allowed at all: on a single node
// there is nowhere else to put a runner, and with dind a job cannot reach the swarm or
// the host socket, so the risk is contention rather than control.
func CheckPlacement(cfg Config, nodes []runtime.Node) error {
	for _, sel := range cfg.Nodes {
		for _, n := range nodes {
			if !strings.EqualFold(n.Hostname, sel) {
				continue
			}
			if isManagerNode(n) && cfg.AllowManagers {
				continue
			}
			if isManagerNode(n) {
				return fmt.Errorf("%w: %q is a %s node", ErrManagerNode, n.Hostname, roleWord(n))
			}
		}
	}
	return nil
}

// ResolveAssignment produces the per-node answer from the cluster settings and
// the live node list. The rules are the reconcile's, in the same order:
//
//	disabled feature                  → disabled answer, with the reason
//	no org / no node list             → refusal (the setting is half-configured)
//	node not in github_runner_nodes   → disabled answer, with the reason
//	any selected node is a manager    → refusal (a build there takes the API down)
//
// The group-visibility check is NOT here: it needs GitHub, and the handler adds
// it right before it mints a registration token.
func ResolveAssignment(cfg Config, nodes []runtime.Node, node string) (Assignment, error) {
	node = strings.TrimSpace(node)
	// The answer is for THIS node, so the selection is evaluated against it. The
	// caller passing a different node than cfg.Node would otherwise get "not
	// selected" for a node that IS selected.
	cfg.Node = node
	a := Assignment{
		Node:              node,
		Org:               cfg.Org,
		Group:             cfg.Group,
		NodeDockerRestart: cfg.NodeDockerRestart,
		// The cap travels RESOLVED FOR THIS NODE: a worker cannot read
		// github_runner_node_memory (configs are manager-only), so if the
		// leader sent the cluster default the node would start the container
		// with a cap the leader had just refused.
		Memory:          cfg.CapFor(node),
		Mode:            ModeEphemeral,
		Labels:          Labels,
		IntervalSeconds: cfg.IntervalSeconds,
	}
	if !cfg.Enabled {
		a.Enabled = false
		if a.Reason == "" {
			a.Reason = "github_runners_enabled=false"
		}
		return a, nil
	}
	switch {
	case strings.TrimSpace(cfg.Org) == "":
		return a, ErrNoOrg
	case len(cfg.Nodes) == 0:
		return a, ErrNoNodes
	}
	if !cfg.WatchesNode() {
		a.Reason = fmt.Sprintf("this node is not in github_runner_nodes (%s)", strings.Join(cfg.Nodes, ","))
		return a, nil
	}
	if err := CheckPlacement(cfg, nodes); err != nil {
		return a, err
	}
	a.Enabled = true
	if a.Reason == "" {
		a.Reason = "selected as a runner node"
	}
	return a, nil
}

// AssignmentService serves the assignment endpoint. It is the leader's side only:
// the node is taken from the request's TLS identity (see internal/auth/swarmtls),
// never from the query string.
type AssignmentService struct {
	// Config reads the current runner settings on THIS node (the leader).
	Config func(ctx context.Context) Config
	// Nodes reads the live swarm node list.
	Nodes swarmtls.NodeLister
	// GitHub mints the registration token and verifies the runner group.
	// Optional: nil makes an enabled answer a refusal (nothing could register).
	GitHub GitHub
	// Deregister removes a registration by name. Set when GitHub is set; the
	// leader de-registers for a node it no longer assigns, because a worker has
	// no App installation and must not be given one.
	Deregister func(ctx context.Context, org, name string) error
	// Held reports whether an operator stopped this runner from the console.
	// Optional: nil means no roster is available and nothing is ever held.
	Held func(runnerName string) bool
	// NodeMemory returns a node's total RAM in MiB (known=false when there is no
	// recent sample). Optional: nil skips the size check, which is what a leader
	// without a store does. See internal/runners/memory.go.
	NodeMemory func(ctx context.Context, node string) (mib int64, known bool)
	// Interval is how long a disabled node's registration is left alone between
	// re-checks (default assignmentDeregisterInterval).
	Interval time.Duration
	Now      func() time.Time
	Log      zerolog.Logger

	mu        sync.Mutex
	lastSweep map[string]time.Time
	// unknownRAM records nodes already told that their RAM is unknown, so the
	// note appears once per leader process instead of once per poll.
	unknownRAM map[string]bool
}

func (s *AssignmentService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *AssignmentService) interval() time.Duration {
	if s.Interval > 0 {
		return s.Interval
	}
	return assignmentDeregisterInterval
}

// ServeHTTP answers GET with the assignment for the certificate's node.
//
// A caller-supplied `?node=` is IGNORED, not refused: the response is always the
// certificate's own node, so there is no code path in which a query parameter
// selects whose assignment is returned. Refusing would be defensible too, but
// ignoring it means the endpoint has exactly one identity source and no branch
// that could later be "helpfully" widened.
func (s *AssignmentService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed: the assignment channel is read-only"})
		return
	}
	ident, ok := swarmtls.FromContext(r.Context())
	if !ok {
		// The listener always runs the middleware; reaching this without it is a
		// wiring bug, and answering anything would be an unauthenticated read.
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden", "reason": "no authenticated swarm node"})
		return
	}
	node := strings.TrimSpace(ident.Hostname)
	if node == "" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden", "reason": "certificate is not a live swarm node"})
		return
	}

	a, err := s.resolve(r.Context(), node)
	if err != nil {
		// A refusal is the answer, not a crash: the node logs it and does not
		// start a runner. It is a 409 because the stored configuration and the
		// node set disagree, which is what the operator has to fix.
		s.Log.Warn().Err(err).Str("node", node).Msg("runners — assignment refused")
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "node": node})
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// resolve applies the resolution rules plus the two things that need GitHub: the
// `selected` group check (before a token is minted) and the de-registration of a
// node the leader no longer assigns.
func (s *AssignmentService) resolve(ctx context.Context, node string) (Assignment, error) {
	cfg := s.Config(ctx)
	cfg.Node = node
	var nodes []runtime.Node
	if s.Nodes != nil {
		list, err := s.Nodes.NodeList(ctx)
		if err != nil {
			return Assignment{}, fmt.Errorf("read the node list to resolve this node's runner: %w", err)
		}
		nodes = list
	}
	a, err := ResolveAssignment(cfg, nodes, node)
	if err != nil {
		return a, err
	}
	a.IssuedAt = s.now().Unix()

	if !a.Enabled {
		s.sweepRegistration(ctx, node, cfg.Org)
		return a, nil
	}
	if s.GitHub == nil {
		return a, fmt.Errorf("%w: this leader has no GitHub App, so no runner can register", ErrNoApp)
	}
	// An operator stop wins over everything below it: no group check, no token,
	// no registration. The node removes any container it still has and stays
	// quiet until the panel's Restart clears the hold.
	if s.Held != nil && s.Held(RunnerName(node)) {
		a.Held = true
		a.Reason = "stopped by an operator from the console — Restart it from the Runners panel to clear"
		return a, nil
	}
	// Then the arithmetic: a cap that claims more than this node can survive is
	// not started at all. Checked here, on EVERY pass and after the hold, so a
	// settings change is re-evaluated rather than remembered — and checked before
	// a token is minted, because a token for a runner that must not run is a
	// registration GitHub would list as offline forever.
	if refused, reason := s.memoryRefusal(ctx, node, a.Memory); refused {
		a.Enabled = false
		a.Refused = true
		a.Reason = reason
		s.sweepRegistration(ctx, node, cfg.Org)
		return a, nil
	}
	// The group is verified on every enabled answer, never only at
	// registration: a visibility flipped to `all` in the GitHub UI must stop the
	// runner on its next poll, not at some later re-registration.
	if err := ensureGroupSelected(ctx, s.GitHub, a.Org, a.Group); err != nil {
		return a, err
	}
	token, err := s.GitHub.RunnerRegistrationToken(ctx, a.Org)
	if err != nil {
		return a, fmt.Errorf("mint a registration token for org %s: %w", a.Org, err)
	}
	a.RegistrationToken = token
	// GitHub's registration tokens live one hour; the node only needs it for the
	// instant it creates the container, and never persists it.
	a.RegistrationTokenExpiresAt = s.now().Add(time.Hour).Unix()
	return a, nil
}

// sweepRegistration de-registers a node's runner name when the leader has told
// that node to stop. The WORKER does not do this: it has no App installation, and
// giving one to every worker would be a far wider credential than this channel
// needs. Bounded to one attempt per node per interval.
func (s *AssignmentService) sweepRegistration(ctx context.Context, node, org string) {
	if s.Deregister == nil || strings.TrimSpace(org) == "" {
		return
	}
	name := RunnerName(node)
	s.mu.Lock()
	if s.lastSweep == nil {
		s.lastSweep = map[string]time.Time{}
	}
	last := s.lastSweep[name]
	if !last.IsZero() && s.now().Sub(last) < s.interval() {
		s.mu.Unlock()
		return
	}
	s.lastSweep[name] = s.now()
	s.mu.Unlock()

	if err := s.Deregister(ctx, org, name); err != nil {
		s.Log.Warn().Err(err).Str("runner", name).Str("org", org).
			Msg("runners — could not de-register a runner on a node the leader stopped")
		return
	}
	s.Log.Info().Str("runner", name).Str("org", org).
		Msg("runners — de-registered a runner on a node the leader stopped")
}

// memoryRefusal applies the size rule to one node's answer.
//
// Two asymmetries, both deliberate:
//   - an UNWIRED lookup (nil NodeMemory) skips the check entirely: the rule needs
//     a node's RAM, and a leader with no store has none — refusing here would make
//     a wiring gap look like a refusal;
//   - UNKNOWN RAM allows the runner and logs ONCE per node per process. A node
//     whose telemetry has not arrived must still be able to run CI; the note is
//     what stops that from being silent.
func (s *AssignmentService) memoryRefusal(ctx context.Context, node, memory string) (bool, string) {
	if s.NodeMemory == nil {
		return false, ""
	}
	mib, known := s.NodeMemory(ctx, node)
	err := CheckMemoryFit(node, mib, known, memory)
	if err != nil {
		s.Log.Warn().Err(err).Str("node", node).Str("memory", memory).
			Msg("runners — refused: this node cannot host a runner with that memory cap")
		return true, fmt.Sprintf("refused: %v", err)
	}
	if !known {
		s.mu.Lock()
		if s.unknownRAM == nil {
			s.unknownRAM = map[string]bool{}
		}
		first := !s.unknownRAM[node]
		s.unknownRAM[node] = true
		s.mu.Unlock()
		if first {
			s.Log.Info().Str("node", node).Str("memory", memory).
				Msg("runners — node RAM unknown (no recent node-stats sample): allowing the runner, memory cap NOT checked")
		}
	}
	return false, ""
}

// ensureGroupSelected refuses a group whose visibility is not `selected`. It is
// the same check the reconcile performs before starting a container.
func ensureGroupSelected(ctx context.Context, gh GitHub, org, group string) error {
	if gh == nil {
		return ErrNoApp
	}
	g, err := gh.EnsureRunnerGroup(ctx, org, group)
	if err != nil {
		if errors.Is(err, githubapp.ErrRunnerGroupVisibility) {
			return fmt.Errorf("refusing to run a runner in org %s: runner group %q is visible to the whole organisation, so workflow code from ANY repository — including one an attacker can push to — would run on a container that can reach a docker daemon (root-equivalent on the host). Set the group's visibility to 'selected' in the org's Actions settings; the platform creates it that way and never flips it", org, group)
		}
		return fmt.Errorf("verify runner group %q in org %s: %w", group, org, err)
	}
	if g.Visibility != "" && g.Visibility != "selected" {
		return fmt.Errorf("refusing to run a runner in org %s: runner group %q reported visibility %q", org, group, g.Visibility)
	}
	return nil
}
