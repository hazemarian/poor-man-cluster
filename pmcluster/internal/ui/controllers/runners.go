package controllers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runners"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

// runtimeNode narrows the API's node view to the fields the eligibility rule
// needs, so the rule itself lives in one place (runners.RolesOf) instead of being
// re-implemented by the console.
func runtimeNode(n pmapi.Node) runtime.Node {
	return runtime.Node{Hostname: n.Hostname, Role: n.Role, IsLeader: n.IsLeader}
}

// roleLabel is the badge's text: the leader is a manager, and saying so avoids a
// picker where "manager" and "leader" look like different failure modes.
func roleLabel(n pmapi.Node) string {
	if n.IsLeader {
		return "leader"
	}
	return n.Role
}

func timeSince(epoch int64) time.Duration { return time.Since(time.Unix(epoch, 0)) }

// Runners is the self-hosted runner panel: the node picker that decides where
// runners run, and the table of the runners that exist with a kill (and restart)
// per row.
//
// The picker writes github_runner_nodes through the same settings API the CLI uses,
// so a manager cannot be selected from here even if the HTML were tampered with:
// the daemon's settings adapter and the runner reconcile both refuse it.
type Runners struct{ *Controller }

// runnerRow is one row of the table, ready to render.
type runnerRow struct {
	Name string
	Org  string
	Node string

	State string
	// StateClass is the pill class (good/warn/bad) — the states here are not the
	// ones statusPill knows, and a wrong colour on "failed" hides a failure.
	StateClass string
	Mode       string

	RegisteredAt int64
	LastSeen     int64
	ContainerID  string

	LastJobURL   string
	LastJobLabel string
	// Reason is why this row is in its state when that is not GitHub's news —
	// a memory refusal names the node's RAM and the cap.
	Reason string

	// Age is how long ago the runner was registered ("12m", "3h", "2d").
	Age string
	// Busy means the kill cancels a running workflow and needs a confirmation.
	Busy bool
}

// runnerNode is one node in the picker.
type runnerNode struct {
	Hostname string
	Role     string
	// Reason explains why a node cannot be chosen; empty means it can.
	Reason     string
	Selectable bool
	// Caution is set on a row that MAY be chosen but carries a cost — the leader with
	// `github_runner_allow_leader` on, where a job competes with the control plane.
	// Rendered as a warning pill rather than a disabled row, because "allowed and
	// contended" and "not allowed" are different facts.
	Caution  string
	Selected bool

	// RunnerState is what this node's runner is doing (none when it has none).
	RunnerState string
	MemTotalMB  int64
	MemAvailMB  int64

	// CapOverride is this node's github_runner_node_memory entry, as stored
	// (empty: the node uses the cluster default, which the input shows as its
	// placeholder).
	CapOverride string
	// Cap is the EFFECTIVE cap for this node: the override when there is one,
	// else the cluster default. It is what the row states, so an operator never
	// has to decide which of "value or placeholder" a runner is running with.
	Cap string
	// RefusedReason is why nothing is running on a selected node — the
	// memory-fit refusal, naming the node's RAM and the cap it was given.
	RefusedReason string
}

type runnersData struct {
	Configured bool
	Known      bool
	Enabled    bool

	ErrKey string
	ErrRaw string
	MsgKey string
	MsgArg string

	Org   string
	Group string
	// Memory is the CLUSTER-WIDE default cap (github_runner_memory); a node
	// with a per-node override shows its own cap in its row.
	Memory string

	Rows  []runnerRow
	Nodes []runnerNode
	// Selected is how many nodes are chosen (the capacity model: N nodes = N
	// runners).
	Selected int
}

func (d *runnersData) fail(key, raw string) { d.ErrKey, d.ErrRaw = key, raw }

// allowLeader reports whether the operator lifted the leader placement rule.
//
// The picker has to agree with the daemon about this, or the console offers a checkbox
// the server refuses (or hides one it would accept). Same question, same setting, read
// here as well as in the daemon.
func (c Runners) allowLeader(ctx context.Context) bool {
	s, err := c.API.ClusterSettings(ctx)
	if err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(s["github_runner_allow_leader"]), "true")
}

// Page renders the panel: the picker, then the table.
func (c Runners) Page(g *gin.Context) {
	c.render(g, runnersData{})
}

// nodeMemoryFieldPrefix names one picker row's memory input:
// `node_memory_<hostname>`. The hostname is in the field name (not a positional
// index) so a row cannot be shifted onto another node by reordering the markup,
// and an unknown host is refused rather than written.
const nodeMemoryFieldPrefix = "node_memory_"

// SaveNodes writes the picker's selection to github_runner_nodes and the
// per-row caps to github_runner_node_memory, in one submit — the two settings
// are one decision ("where does a runner run, and how big is it there"), and
// saving one without the other would leave a cap that describes nothing.
func (c Runners) SaveNodes(g *gin.Context) {
	d := runnersData{}
	if c.apiRefused(g, &d) {
		c.render(g, d)
		return
	}
	allowLeader := c.allowLeader(g.Request.Context())
	chosen := map[string]bool{}
	for _, n := range g.PostFormArray("node") {
		chosen[strings.ToLower(strings.TrimSpace(n))] = true
	}
	nodes, err := c.API.Nodes(g.Request.Context())
	if err != nil {
		d.fail("runners.err_nodes", err.Error())
		c.render(g, d)
		return
	}
	// The placement rule is applied here as well as in the UI: the picker posts
	// checkboxes, and a request forged with a manager's hostname must not reach the
	// setting. Both sides ask the same function with the same allow-leader flag, so the
	// console cannot offer a row the server refuses.
	var kept []string
	for _, n := range nodes {
		if !chosen[strings.ToLower(n.Hostname)] {
			continue
		}
		if ok, reason := runners.RolesOf(runtimeNode(n), allowLeader); !ok {
			d.fail("runners.err_manager", reason)
			c.render(g, d)
			return
		}
		kept = append(kept, n.Hostname)
	}
	overrides, errKey, errRaw := c.nodeMemoryOverrides(g, nodes)
	if errKey != "" {
		d.fail(errKey, errRaw)
		c.render(g, d)
		return
	}
	if _, err := c.API.UpdateClusterSettings(g.Request.Context(), map[string]string{
		"github_runner_nodes":       strings.Join(kept, ","),
		"github_runner_node_memory": runners.FormatNodeMemory(overrides),
	}); err != nil {
		d.fail("runners.err_save", err.Error())
		c.render(g, d)
		return
	}
	if len(kept) == 0 {
		d.MsgKey = "runners.msg_saved_none"
	} else {
		d.MsgKey, d.MsgArg = "runners.msg_saved", strings.Join(kept, ", ")
	}
	c.render(g, d)
}

// nodeMemoryOverrides reads every `node_memory_<host>` field from the same
// submit. A blank value means "no override" (the cluster default applies), which
// is how an operator removes one; an unknown host, a manager, or a quantity the
// rest of the platform would refuse is an error naming the field, not a value
// silently dropped.
func (c Runners) nodeMemoryOverrides(g *gin.Context, nodes []pmapi.Node) (map[string]string, string, string) {
	if err := g.Request.ParseForm(); err != nil {
		return nil, "runners.err_memory", err.Error()
	}
	byHost := make(map[string]pmapi.Node, len(nodes))
	for _, n := range nodes {
		byHost[strings.ToLower(n.Hostname)] = n
	}
	overrides := map[string]string{}
	for field, values := range g.Request.PostForm {
		if !strings.HasPrefix(field, nodeMemoryFieldPrefix) {
			continue
		}
		host := strings.TrimSpace(strings.TrimPrefix(field, nodeMemoryFieldPrefix))
		raw := ""
		if len(values) > 0 {
			raw = strings.TrimSpace(values[len(values)-1])
		}
		if raw == "" {
			continue
		}
		node, ok := byHost[strings.ToLower(host)]
		if !ok {
			return nil, "runners.err_unknown_node", host
		}
		if ok, reason := runners.RolesOf(runtimeNode(node), c.allowLeader(g.Request.Context())); !ok {
			return nil, "runners.err_manager", reason
		}
		if err := runners.ValidateMemory(raw); err != nil {
			return nil, "runners.err_memory", fmt.Sprintf("%s: %v", node.Hostname, err)
		}
		overrides[node.Hostname] = raw
	}
	return overrides, "", ""
}

// Kill removes one runner's container and its registration. A busy runner needs
// the row's confirmation, which the template asks for and this handler forwards.
func (c Runners) Kill(g *gin.Context) {
	c.act(g, "runners.err_kill", func(ctx context.Context, name string) (*pmapi.GitHubRunnerKill, error) {
		return c.API.KillGitHubRunner(ctx, name, g.PostForm("confirm") == "1")
	})
}

// Restart kills and re-registers one runner.
func (c Runners) Restart(g *gin.Context) {
	c.act(g, "runners.err_restart", func(ctx context.Context, name string) (*pmapi.GitHubRunnerKill, error) {
		return c.API.RestartGitHubRunner(ctx, name, g.PostForm("confirm") == "1")
	})
}

// RestartAll re-registers every runner the panel shows as stopped — the
// panel-level Restart. It is a loop of single restarts, so one failure does not
// stop the others and the message names how many worked.
func (c Runners) RestartAll(g *gin.Context) {
	d := runnersData{}
	if c.apiRefused(g, &d) {
		c.render(g, d)
		return
	}
	view, err := c.API.GitHubRunners(g.Request.Context())
	if err != nil {
		d.fail("runners.err_list", err.Error())
		c.render(g, d)
		return
	}
	restarted, failed := 0, 0
	for _, r := range view.Runners {
		if r.State != runners.StateOffline {
			continue
		}
		if _, err := c.API.RestartGitHubRunner(g.Request.Context(), r.Name, false); err != nil {
			failed++
			continue
		}
		restarted++
	}
	if failed > 0 {
		d.fail("runners.err_restart_partial", fmt.Sprintf("%d restarted, %d failed", restarted, failed))
	} else {
		d.MsgKey = "runners.msg_restarted_all"
		d.MsgArg = fmt.Sprint(restarted)
	}
	c.render(g, d)
}

// act runs one kill/restart and maps the API's two actionable statuses to a
// translated message.
func (c Runners) act(g *gin.Context, errKey string, call func(context.Context, string) (*pmapi.GitHubRunnerKill, error)) {
	d := runnersData{}
	name := strings.TrimSpace(g.Param("name"))
	switch {
	case c.apiRefused(g, &d):
		c.render(g, d)
		return
	case name == "":
		d.fail("runners.err_bad_name", "")
		c.render(g, d)
		return
	}
	out, err := call(g.Request.Context(), name)
	if err != nil {
		var apiErr *pmapi.Error
		switch {
		case errors.As(err, &apiErr) && apiErr.Status == 404:
			d.fail("runners.err_not_found", apiErr.Body)
		case errors.As(err, &apiErr) && apiErr.Status == 409:
			d.fail("runners.err_busy", apiErr.Body)
		default:
			d.fail(errKey, err.Error())
		}
		c.render(g, d)
		return
	}
	switch {
	case out.DeregisterError != "":
		// Half a kill is reported as half a kill: the container is gone, and the
		// registration is the thing still out there.
		d.MsgKey, d.MsgArg = errKey+"_deregister", out.DeregisterError
	case out.RegistrationError != "":
		d.MsgKey, d.MsgArg = errKey+"_register", out.RegistrationError
	case strings.Contains(errKey, "restart"):
		d.MsgKey, d.MsgArg = "runners.msg_restarted", name
	default:
		d.MsgKey, d.MsgArg = "runners.msg_killed", name
	}
	c.render(g, d)
}

// render loads the panel's data and renders the fragment. A failed list call is
// its own state (Known=false): "we could not ask" is not "there are none".
func (c Runners) render(g *gin.Context, d runnersData) {
	ctx := g.Request.Context()
	if _, _, configured := c.loadParams(ctx); !configured {
		c.Views.Fragment(g, "runners", d)
		return
	}
	d.Configured = true

	settings, serr := c.API.ClusterSettings(ctx)
	if serr != nil && d.ErrKey == "" {
		d.fail("runners.err_settings", serr.Error())
	}
	d.Org = settings["github_runner_org"]
	d.Group = settings["github_runner_group"]
	// One settings view for the whole panel: the effective cap of every row is
	// resolved by the same function the leader uses on the assignment, so the
	// number the picker shows is the number the container will get.
	capCfg := runners.Config{
		Memory:     settings["github_runner_memory"],
		NodeMemory: runners.ParseNodeMemory(settings["github_runner_node_memory"]),
	}
	d.Memory = capCfg.DefaultCap()
	selected := map[string]bool{}
	for _, n := range runners.ParseNodes(settings["github_runner_nodes"]) {
		selected[strings.ToLower(n)] = true
	}

	view, err := c.API.GitHubRunners(ctx)
	if err != nil {
		if d.ErrKey == "" {
			d.fail("runners.err_list", err.Error())
		}
		c.Views.Fragment(g, "runners", d)
		return
	}
	d.Known, d.Enabled = true, view.Enabled
	d.Rows = buildRunnerRows(view.Runners)
	d.Selected = 0
	for _, r := range d.Rows {
		if selected[strings.ToLower(r.Node)] {
			d.Selected++
		}
	}

	// The picker. Nodes and their memory come from the daemon; a failure here
	// leaves the table usable and the picker empty with the error banner already
	// set.
	nodes, nerr := c.API.Nodes(ctx)
	if nerr != nil {
		if d.ErrKey == "" {
			d.fail("runners.err_nodes", nerr.Error())
		}
		c.Views.Fragment(g, "runners", d)
		return
	}
	stats, _ := c.API.NodeStats(ctx)
	mem := map[string]pmapi.NodeStat{}
	for _, s := range stats {
		mem[strings.ToLower(s.Hostname)] = s
	}
	stateByNode := map[string]string{}
	reasonByNode := map[string]string{}
	for _, r := range d.Rows {
		stateByNode[strings.ToLower(r.Node)] = r.State
		if r.Reason != "" {
			reasonByNode[strings.ToLower(r.Node)] = r.Reason
		}
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Role != nodes[j].Role {
			return nodes[i].Role < nodes[j].Role
		}
		return nodes[i].Hostname < nodes[j].Hostname
	})
	for _, n := range nodes {
		ok, reason := runners.RolesOf(runtimeNode(n), c.allowLeader(g.Request.Context()))
		// A selectable row can still carry a caution: "allowed, and here is the cost".
		caution := ""
		if ok {
			caution, reason = reason, ""
		}
		st := mem[strings.ToLower(n.Hostname)]
		key := strings.ToLower(n.Hostname)
		d.Nodes = append(d.Nodes, runnerNode{
			Hostname:      n.Hostname,
			Role:          roleLabel(n),
			Reason:        reason,
			Selectable:    ok,
			Caution:       caution,
			Selected:      selected[key],
			RunnerState:   stateByNode[key],
			MemTotalMB:    st.MemTotalMB,
			MemAvailMB:    st.MemAvailMB,
			CapOverride:   capCfg.NodeMemory[key],
			Cap:           capCfg.CapFor(n.Hostname),
			RefusedReason: reasonByNode[key],
		})
	}
	c.Views.Fragment(g, "runners", d)
}

// buildRunnerRows projects the API rows into the table's view model.
func buildRunnerRows(in []pmapi.GitHubRunner) []runnerRow {
	out := make([]runnerRow, 0, len(in))
	for _, r := range in {
		row := runnerRow{
			Name: r.Name, Org: r.Org, Node: r.Node, Mode: r.Mode,
			State: r.State, StateClass: stateClass(r.State),
			RegisteredAt: r.RegisteredAt, LastSeen: r.LastSeen,
			ContainerID: r.ContainerID, LastJobURL: r.LastJobURL, Reason: r.Reason,
			LastJobLabel: lastJobLabel(r.LastJobURL),
			Age:          ageLabel(r.RegisteredAt),
			Busy:         r.State == runners.StateBusy,
		}
		out = append(out, row)
	}
	return out
}

// stateClass maps a runner state to the design system's pill class.
func stateClass(state string) string {
	switch state {
	case runners.StateIdle:
		return "good"
	case runners.StateBusy, runners.StateStarting:
		return "warn"
	case runners.StateBetweenJobs:
		// Not a fault: an ephemeral runner has no container between jobs.
		return "plain"
	default: // offline, failed
		return "bad"
	}
}

// ageLabel renders how long ago a runner was registered. A zero timestamp is an
// em dash: the panel must not claim "0s" for a runner it never saw register.
func ageLabel(epoch int64) string {
	if epoch <= 0 {
		return "—"
	}
	d := timeSince(epoch)
	switch {
	case d < 0:
		return "—"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// lastJobLabel shortens a run URL to "owner/repo#run", which fits the column and
// says which repository the job belonged to.
func lastJobLabel(url string) string {
	if url == "" {
		return ""
	}
	trimmed := strings.TrimSuffix(strings.TrimPrefix(url, "https://github.com/"), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 {
		return trimmed
	}
	label := parts[0] + "/" + parts[1]
	if len(parts) >= 2 {
		label += "#" + parts[len(parts)-1]
	}
	return label
}
