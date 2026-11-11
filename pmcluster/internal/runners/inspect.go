package runners

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// RequiredPermission is the App permission runner registration needs.
//
// It is NOT `administration`. Organization-level runner endpoints — the runner
// list, the runner groups, the registration token — require
// `organization_self_hosted_runners` at `write`; `administration` only covers
// the repository-level equivalents. An App can hold
// `administration: write` AND `organization_administration: write` and still be
// refused here, which is exactly what happened on a live cluster: the
// diagnostic blamed a permission that was already granted and sent the operator
// looking in the wrong place for an evening.
//
// GitHub's failure body is a bare 403 ("Resource not accessible by
// integration") naming neither the permission nor the fix, which is why the
// doctor reports this before a registration is ever attempted.
const RequiredPermission = "organization_self_hosted_runners:write"

// Report is the doctor's view: enabled or not, per selected node whether the
// container and the GitHub registration agree, and — when the feature is off —
// what the disable left behind.
type Report struct {
	Enabled bool `json:"enabled"`
	// AppConfigured is false when no GitHub App registration exists, which
	// makes an enabled feature inert (and is reported as a failure: enabling it
	// is refused without an App).
	AppConfigured bool   `json:"app_configured"`
	Org           string `json:"org,omitempty"`
	Group         string `json:"group,omitempty"`
	// MissingPermission names RequiredPermission when the App's granted set
	// does not include it.
	MissingPermission string       `json:"missing_permission,omitempty"`
	Nodes             []NodeReport `json:"nodes,omitempty"`
	// Leftovers are containers, bookkeeping entries or rows that still exist
	// while the feature is OFF. They are the failure mode of a half-disable:
	// something kept a docker socket or a registration alive.
	Leftovers []string `json:"leftovers,omitempty"`
	// Unmanaged are pmc-named runners GitHub knows that this platform does not
	// manage for any selected node — a hand-made runner. Named, never killed,
	// never counted as healthy.
	Unmanaged []RunnerNote `json:"unmanaged,omitempty"`
	// Warnings are failures Inspect could not attribute to one node.
	Warnings []string `json:"warnings,omitempty"`
	// DefaultMemory is the cluster-wide github_runner_memory, carried so a
	// reader can tell a per-node override from the default without the
	// settings snapshot.
	DefaultMemory string `json:"default_memory,omitempty"`
	// StaleMemoryNodes are hosts named by github_runner_node_memory that are
	// not in github_runner_nodes. They are KEPT (a node may be temporarily
	// deselected, and its cap should be waiting when it comes back) but the
	// doctor names them, because an override for a node that runs nothing is
	// usually a rename the operator has not finished.
	StaleMemoryNodes []string `json:"stale_memory_nodes,omitempty"`
}

// RunnerNote is one runner GitHub reports that the platform does not own.
type RunnerNote struct {
	Name string `json:"name"`
	Busy bool   `json:"busy"`
	// RawSocket is set when this node could inspect the container and found the
	// host's socket bound. A runner on another node is unknowable from here.
	RawSocket   bool `json:"raw_socket"`
	SocketKnown bool `json:"socket_known"`
}

// NodeReport is one selected node's state.
type NodeReport struct {
	Node     string `json:"node"`
	Role     string `json:"role,omitempty"`
	Selected bool   `json:"selected"`
	// ContainerRunning is this node's local runner container.
	ContainerRunning bool `json:"container_running"`
	// ContainerKnown says whether ContainerRunning was actually observed. Only
	// the daemon that RUNS on the node can inspect its containers, so from the
	// leader every other node's value is unknown — not false. Without this the
	// two were indistinguishable, and `doctor` reported "registered but no
	// container is running" for every healthy worker on the fleet, because
	// false-means-unknown was read as false-means-absent.
	ContainerKnown bool   `json:"container_known"`
	Registered     bool   `json:"registered"`
	RunnerName     string `json:"runner_name"`
	// DockerAccess is "socket proxy" or "raw socket": the exposure a job gets.
	DockerAccess string `json:"docker_access,omitempty"`
	// RawSocket is true when the local container binds /var/run/docker.sock.
	RawSocket bool `json:"raw_socket"`
	// Offline lists registrations GitHub calls offline (a runner that will never
	// pick up a job until it starts).
	Offline   []string `json:"offline,omitempty"`
	Failures  int      `json:"consecutive_failures,omitempty"`
	LastError string   `json:"last_error,omitempty"`
	Error     string   `json:"error,omitempty"`
	// Refused and Reason carry the size refusal to `doctor` and the console. A
	// refusal is not an error: nothing is broken, the cap simply does not fit
	// this node, and the reason names both numbers.
	Refused bool   `json:"refused,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Cap is the memory cap resolved FOR THIS NODE: its
	// github_runner_node_memory override when one exists, else the cluster
	// default. The doctor and the console report this, so an override is
	// visible without reading the setting.
	Cap string `json:"cap,omitempty"`
}

// Inspect reads the runner state without changing anything. When the feature is
// disabled it makes no GitHub calls at all: leftovers are provable locally (a
// container with our label, a bookkeeping entry, a row), which is exactly the
// evidence a half-disable leaves.
func (m *Manager) Inspect(ctx context.Context) (Report, error) {
	cfg := LoadConfig(ctx, m.Store, m.Node)
	rep := Report{Enabled: cfg.Enabled, Org: cfg.Org, Group: cfg.Group, DefaultMemory: cfg.DefaultCap()}
	state := loadState(ctx, m.Store)

	// An override for a host that is not in github_runner_nodes caps nothing.
	// It is kept (the node may come back) and named here so a stale rename does
	// not hide behind a setting that looks configured.
	selected := map[string]bool{}
	for _, n := range cfg.Nodes {
		selected[strings.ToLower(strings.TrimSpace(n))] = true
	}
	for host := range cfg.NodeMemory {
		if !selected[host] {
			rep.StaleMemoryNodes = append(rep.StaleMemoryNodes, host)
		}
	}
	sort.Strings(rep.StaleMemoryNodes)

	if !cfg.Enabled {
		rep.Leftovers = m.localLeftovers(ctx, state)
		return rep, nil
	}
	if m.GitHub == nil {
		rep.Warnings = append(rep.Warnings, "no GitHub App is configured, so runners cannot be registered (pmcluster github app set)")
		return rep, nil
	}
	rep.AppConfigured = true
	if strings.TrimSpace(cfg.Org) == "" {
		rep.Warnings = append(rep.Warnings, "github_runner_org is empty, so no runner can register")
		return rep, nil
	}
	if len(cfg.Nodes) == 0 {
		rep.Warnings = append(rep.Warnings, "github_runner_nodes is empty, so no runner will run anywhere")
		return rep, nil
	}

	if perms, err := m.GitHub.GrantedPermissions(ctx); err != nil {
		rep.Warnings = append(rep.Warnings, "could not read the App's granted permissions: "+err.Error())
	} else if levelRank(perms["administration"]) < levelRank("write") {
		rep.MissingPermission = RequiredPermission
	}

	// The org's registrations, by name. An unreachable GitHub is reported as a
	// warning, never as "every runner is offline".
	remote := map[string]githubappRunner{}
	if list, err := m.GitHub.ListRunners(ctx, cfg.Org); err != nil {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("list runners for org %s: %v", cfg.Org, err))
	} else {
		for _, r := range list {
			remote[r.Name] = githubappRunner{status: r.Status, busy: r.Busy}
		}
	}

	roles := m.nodeRoles(ctx, &rep)
	expected := map[string]bool{}
	for _, node := range cfg.Nodes {
		name := RunnerName(node)
		expected[name] = true
		nr := NodeReport{Node: node, Selected: true, RunnerName: name, Role: roles[strings.ToLower(node)], Cap: cfg.CapFor(node)}
		if refused, rerr := m.memoryRefusal(ctx, node, nr.Cap); refused {
			// A refusal is the first thing an operator needs to see about this
			// node: no container will start, and the reason names both numbers.
			nr.Refused, nr.Reason = true, rerr.Error()
			rep.Nodes = append(rep.Nodes, nr)
			continue
		}
		if e, ok := state.entry(name); ok {
			nr.Failures = e.Failures
			nr.LastError = e.LastError
		}
		if strings.EqualFold(node, m.Node) {
			nr.DockerAccess = m.dockerAccess(cfg)
			if c, ok, err := m.inspect(ctx, name); err != nil {
				nr.Error = err.Error()
			} else if ok {
				nr.ContainerKnown = true
				nr.ContainerRunning = c.Running()
				nr.RawSocket = c.HasRawSocketMount()
				if nr.RawSocket {
					nr.DockerAccess = "raw socket"
				}
			}
		}
		if r, ok := remote[name]; ok {
			nr.Registered = true
			// "offline" is NOT a fault for this platform's runners: they are
			// EPHEMERAL, so the container exits 0 after each job, the reconcile
			// builds a new one, and GitHub reports the gap as offline. Measured on a
			// healthy worker: container "Up 15 seconds", log "Running job", while
			// doctor said "offline or stale registration".
			//
			// Only report it when the container was OBSERVED not running (this daemon
			// runs on that node). From anywhere else the state is unobservable, and an
			// unobservable gap must stay silent — the same rule the container checks
			// follow, and the reason a warning that fires on every healthy node is a
			// warning nobody reads.
			if !strings.EqualFold(r.status, "online") && nr.ContainerKnown && !nr.ContainerRunning {
				nr.Offline = append(nr.Offline, name)
			}
		}
		rep.Nodes = append(rep.Nodes, nr)
	}

	// pmc-named registrations this platform does not manage: the operator's
	// hand-made runner, or a leftover from a node that was removed from the list.
	for name, r := range remote {
		if !strings.HasPrefix(name, RunnerNamePrefix) || expected[name] {
			continue
		}
		note := RunnerNote{Name: name, Busy: r.busy}
		// A local container of that name can still be inspected for its mounts.
		if c, ok, err := m.inspect(ctx, name); err == nil && ok {
			note.SocketKnown = true
			note.RawSocket = c.HasRawSocketMount()
		}
		rep.Unmanaged = append(rep.Unmanaged, note)
	}
	return rep, nil
}

type githubappRunner struct {
	status string
	busy   bool
}

// localLeftovers collects what a disabled feature left on this node: a runner
// container, a bookkeeping entry, or a row. It makes no API call, so it works
// with GitHub unreachable — which is exactly the state a failed disable leaves.
func (m *Manager) localLeftovers(ctx context.Context, state State) []string {
	var out []string
	name := RunnerName(m.Node)
	if m.Docker != nil {
		if c, ok, err := m.Docker.Inspect(ctx, name); err == nil && ok {
			out = append(out, fmt.Sprintf("container %s (%s)", name, c.State))
		}
	}
	for _, e := range state.Runners {
		out = append(out, fmt.Sprintf("registration record for %s in org %s", e.Name, e.Org))
	}
	if m.Store != nil {
		if rows, err := m.Store.ListGitHubRunners(ctx, 200); err == nil && len(rows) > 0 {
			names := make([]string, 0, len(rows))
			for _, r := range rows {
				names = append(names, r.Name)
			}
			out = append(out, "runner row(s) "+strings.Join(names, ", "))
		}
	}
	return out
}

// memoryRefusal is the Manager's half of the size rule: the same CheckMemoryFit
// the leader's assignment resolver applies, so `doctor` and the console cannot
// disagree with the decision that actually stopped the container.
//
// nil NodeMemory (a manager built without a store) or unknown RAM allows: the rule
// refuses on arithmetic, never on missing information.
func (m *Manager) memoryRefusal(ctx context.Context, node, memory string) (bool, error) {
	if m.NodeMemory == nil {
		return false, nil
	}
	mib, known := m.NodeMemory(ctx, node)
	if err := CheckMemoryFit(node, mib, known, memory); err != nil {
		return true, err
	}
	return false, nil
}

// nodeRoles maps lowercased hostname -> role for the selected nodes.
func (m *Manager) nodeRoles(ctx context.Context, rep *Report) map[string]string {
	out := map[string]string{}
	if m.Nodes == nil {
		return out
	}
	list, err := m.Nodes.NodeList(ctx)
	if err != nil {
		rep.Warnings = append(rep.Warnings, "could not read the node list: "+err.Error())
		return out
	}
	for _, n := range list {
		out[strings.ToLower(n.Hostname)] = roleWord(n)
	}
	return out
}

// dockerAccess is the exposure a local runner has.
func (m *Manager) dockerAccess(cfg Config) string {
	if cfg.SocketProxy {
		return "socket proxy"
	}
	return "raw socket"
}

// levelRank orders GitHub permission levels the way the App's granted set is
// reported: absent < read < write < admin.
func levelRank(level string) int {
	switch strings.ToLower(level) {
	case "read":
		return 1
	case "write":
		return 2
	case "admin":
		return 3
	}
	return 0
}

// RolesOf is a small helper the console's node picker uses to render a node's
// eligibility. It returns (isWorker, reason).
func RolesOf(n runtime.Node) (bool, string) {
	if n.IsLeader {
		return false, "the swarm leader is the control plane — a build here marks the node Down"
	}
	if strings.EqualFold(n.Role, "manager") {
		return false, "managers run the control plane (1 vCPU) — a build here marks the node Down"
	}
	return true, ""
}
