package mcpserver

// Curated tools: the handful of questions that are not one REST operation.
//
// Everything else is derived from the OpenAPI document, but "is the cluster
// healthy", "how much is left", and "what am I allowed to change" each need
// several endpoints combined into one answer, and "run this on the host" has no
// endpoint at all. Their schemas are written here by hand, which is why they are
// a short, fixed list rather than a second way of doing everything.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const emptySchema = `{"type":"object","properties":{},"additionalProperties":false}`

// manualActions are the operations an agent must never attempt and must hand
// back to the operator: they need a shell, root, or a node that is not yet part
// of a cluster, so no API call can stand in for them.
var manualActions = map[string]struct{ command, where, why string }{
	"cluster_up": {
		command: "pmcluster cluster up",
		where:   "on the manager node that will seed the cluster",
		why:     "bootstraps a new cluster; the daemon cannot bootstrap the cluster it is running inside",
	},
	"cluster_reset": {
		command: "pmcluster cluster reset",
		where:   "on the manager node",
		why:     "destroys cluster state; requires an interactive confirmation on the host",
	},
	"join_node": {
		command: "pmcluster node join-token worker   # then, on the new host: pmcluster join <manager> --token …",
		where:   "on the new host",
		why:     "the joining host has no cluster identity yet, so it cannot call the API",
	},
	"upgrade": {
		command: "pmcluster cluster update",
		where:   "on the leader (or via pmc_update when the MCP write tier is enabled)",
		why:     "re-applies platform stacks and may restart the daemon doing the work",
	},
	"host_harden": {
		command: "sudo ./harden-host.sh",
		where:   "on the host, as root",
		why:     "changes firewall/SSH policy; no API surface by design",
	},
	"tls_site_set": {
		command: "pmcluster tls site set --cert … --key …",
		where:   "on the leader",
		why:     "materialises versioned swarm secrets and redeploys the edge",
	},
	"secret_heal": {
		command: "pmcluster secret heal",
		where:   "on a node holding the store and ~/.pmcluster/.encryption_key",
		why:     "needs the local encryption key, which never leaves the node",
	},
}

func curatedTools() []Tool {
	actions := make([]string, 0, len(manualActions))
	for k := range manualActions {
		actions = append(actions, k)
	}
	sort.Strings(actions)
	actionEnum, _ := json.Marshal(actions)

	return []Tool{
		{
			Name: "pmc_cluster_status",
			Description: "Cluster health in one answer: leader, node readiness, stacks with a failed last deploy, " +
				"open failover markers, and observability coverage (expected vs reporting nodes). " +
				"Prefer this over calling the individual list endpoints when the question is \"is everything OK\".",
			Tier:        ReadOnly,
			InputSchema: json.RawMessage(emptySchema),
		},
		{
			Name: "pmc_cluster_capacity",
			Description: "What the cluster has to spend: CPU and memory of the answering node, node counts by role " +
				"and readiness, stacks and services running against their desired replicas, and whether telemetry " +
				"is reporting. Use it before scheduling work or when asked how much room is left.",
			Tier:        ReadOnly,
			InputSchema: json.RawMessage(emptySchema),
		},
		{
			Name: "pmc_capabilities",
			Description: "What THIS MCP endpoint and token may do: the enabled tier, and every tool grouped as read, " +
				"write or dangerous. Call it before attempting a change, so you can tell the user 'I can read X but " +
				"you must run Y yourself' instead of discovering it from a failed call.",
			Tier:        ReadOnly,
			InputSchema: json.RawMessage(emptySchema),
		},
		{
			Name: "pmc_manual_action",
			Description: "Return the exact command an operator must run by hand for work no API call can perform " +
				"(bootstrapping a cluster, joining a node, host hardening, TLS materialisation, secret healing). " +
				"Always relay the returned command to the user verbatim instead of trying to work around it.",
			Tier: ReadOnly,
			InputSchema: json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": %s,
               "description": "the manual operation the user needs to run"},
    "detail": {"type": "string", "description": "optional context to pass along, e.g. the node hostname"}
  },
  "required": ["action"],
  "additionalProperties": false
}`, string(actionEnum))),
		},
	}
}

// runCurated dispatches a curated tool.
func runCurated(ctx context.Context, base http.Handler, authz string, t Tool, args map[string]any, catalog []Tool, opts Options) (*mcp.CallToolResult, error) {
	switch t.Name {
	case "pmc_cluster_status":
		return textResult(mustJSON(clusterStatus(ctx, base, authz))), nil
	case "pmc_cluster_capacity":
		return textResult(mustJSON(clusterCapacity(ctx, base, authz))), nil
	case "pmc_capabilities":
		return textResult(mustJSON(capabilities(catalog, opts))), nil
	case "pmc_manual_action":
		action, _ := args["action"].(string)
		detail, _ := args["detail"].(string)
		spec, ok := manualActions[action]
		if !ok {
			return errorResult(fmt.Sprintf("unknown manual action %q", action)), nil
		}
		return manualAction(manualActionPayload{
			Tool:    t.Name,
			Reason:  spec.why,
			Command: spec.command,
			Where:   spec.where + hintDetail(detail),
			TellTheUser: fmt.Sprintf("This has to be done by hand, not through me: run `%s` %s.",
				spec.command, spec.where),
		}), nil
	}
	return errorResult("unknown curated tool " + t.Name), nil
}

func hintDetail(detail string) string {
	if strings.TrimSpace(detail) == "" {
		return ""
	}
	return " (" + detail + ")"
}

func mustJSON(v any) string {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	return string(raw)
}

// clusterStatus combines the four reads a healthy/not-healthy answer needs, and
// degrades honestly: a source that fails is reported as unavailable rather than
// silently omitted.
func clusterStatus(ctx context.Context, base http.Handler, authz string) map[string]any {
	out := map[string]any{}
	sources := map[string]string{}

	info, err := get(ctx, base, authz, "/api/cluster/info")
	if err != nil {
		sources["cluster_info"] = err.Error()
	} else {
		out["swarm"] = info["swarm"]
		out["engine"] = map[string]any{"version": info["server_version"], "os": info["os"], "arch": info["arch"]}
	}

	var leader string
	ready, total := 0, 0
	nodes, err := get(ctx, base, authz, "/api/nodes")
	if err != nil {
		sources["nodes"] = err.Error()
	} else if list, ok := nodes["nodes"].([]any); ok {
		var notReady []string
		for _, n := range list {
			node, _ := n.(map[string]any)
			total++
			if isLeader, _ := node["is_leader"].(bool); isLeader {
				leader, _ = node["hostname"].(string)
			}
			if s, _ := node["status"].(string); s == "ready" {
				ready++
			} else {
				notReady = append(notReady, fmt.Sprint(node["hostname"]))
			}
		}
		out["nodes"] = map[string]any{"ready": ready, "total": total, "not_ready": notReady}
	}
	out["leader"] = leader

	stacks, err := get(ctx, base, authz, "/api/stacks")
	if err != nil {
		sources["stacks"] = err.Error()
	} else if list, ok := stacks["stacks"].([]any); ok {
		var failing []map[string]any
		for _, s := range list {
			st, _ := s.(map[string]any)
			lastErr, _ := st["last_error"].(string)
			failover, _ := st["failover"].(map[string]any)
			if lastErr != "" || len(failover) > 0 {
				failing = append(failing, map[string]any{
					"stack":      st["name"],
					"last_error": lastErr,
					"failover":   failover,
				})
			}
		}
		out["stacks"] = map[string]any{"total": len(list), "needing_attention": failing}
	}

	obs, err := get(ctx, base, authz, "/api/observability/status")
	if err != nil {
		sources["observability"] = err.Error()
	} else {
		out["observability"] = map[string]any{
			"status":           obs["status"],
			"expected_nodes":   obs["expected_nodes"],
			"reporting_nodes":  obs["reporting_nodes"],
			"telemetry_status": obs["telemetry_status"],
		}
	}
	if len(sources) > 0 {
		out["unavailable_sources"] = sources
	}
	return out
}

// clusterCapacity reports what the cluster has: the answering node's CPU/memory
// plus the schedulable inventory. Per-node CPU/memory samples live in the
// metrics endpoint and are reported as a source, not invented here.
func clusterCapacity(ctx context.Context, base http.Handler, authz string) map[string]any {
	out := map[string]any{}
	sources := map[string]string{}

	info, err := get(ctx, base, authz, "/api/cluster/info")
	if err != nil {
		sources["cluster_info"] = err.Error()
	} else {
		out["answering_node"] = map[string]any{
			"node_name":    info["node_name"],
			"cpus":         info["cpus"],
			"memory_bytes": info["memory_bytes"],
		}
	}

	nodes, err := get(ctx, base, authz, "/api/nodes")
	if err != nil {
		sources["nodes"] = err.Error()
	} else if list, ok := nodes["nodes"].([]any); ok {
		byRole := map[string]int{}
		for _, n := range list {
			node, _ := n.(map[string]any)
			role, _ := node["role"].(string)
			if role == "" {
				role = "unknown"
			}
			byRole[role]++
		}
		out["nodes"] = map[string]any{"total": len(list), "by_role": byRole}
	}

	services, err := get(ctx, base, authz, "/api/services")
	if err != nil {
		sources["services"] = err.Error()
	} else if list, ok := services["services"].([]any); ok {
		var desired, running, platform int
		for _, s := range services["services"].([]any) {
			svc, _ := s.(map[string]any)
			if p, _ := svc["platform"].(bool); p {
				platform++
			}
			if d, ok := svc["desired"].(float64); ok {
				desired += int(d)
			}
			if r, ok := svc["replicas"].(float64); ok {
				running += int(r)
			}
		}
		out["workloads"] = map[string]any{
			"services":          len(list),
			"platform_services": platform,
			"desired_replicas":  desired,
			"running_replicas":  running,
		}
	}
	if len(sources) > 0 {
		out["unavailable_sources"] = sources
	}
	return out
}

// capabilities answers "what may I do" without calling anything.
func capabilities(all []Tool, opts Options) map[string]any {
	var writable, blocked []string
	for _, t := range all {
		if t.Tier == ReadOnly {
			continue
		}
		if allowed(opts, t) {
			writable = append(writable, t.Name)
			continue
		}
		blocked = append(blocked, t.Name)
	}
	sort.Strings(writable)
	sort.Strings(blocked)
	return map[string]any{
		"writes_allowed": writable,
		"writes_refused": blocked,
		"rules": map[string]any{
			"reads":     "all read tools are served",
			"writes":    "only the operations on this endpoint's allowlist; everything else answers manual_action_required",
			"dangerous": "never served (exec, secret reveal, deletions, TLS, backup restore)",
			"scope":     "a config update is only permitted when the config's own scope is 'service'",
		},
		"tools": ToolsByTier(all),
		"manual_only": func() []string {
			out := make([]string, 0, len(manualActions))
			for k := range manualActions {
				out = append(out, k)
			}
			sort.Strings(out)
			return out
		}(),
		"note": "Role and stack scope are enforced by the daemon, not by this list: a stack-scoped token " +
			"still sees only its own stack, and a viewer token cannot write. If a call is refused, relay " +
			"the daemon's error and ask the user to run the equivalent command themselves.",
	}
}
