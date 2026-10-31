package mcpserver

// Server-level descriptions: the `instructions` string returned at initialize, and
// read-only resources carrying the schemas (API, logs, metrics, policy) an agent
// would otherwise have to infer from tool output.
//
// Everything here is generated from constants and the embedded spec, so it cannot
// drift from the daemon: the metric names come from the observability package's
// own constants, the write list from the live allowlist, the log projection is
// pinned by a test against the SQL the Parseable adapter actually sends.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

// logColumns is the projection the Parseable adapter selects for log rows. A test
// asserts it matches the SQL built by parseable.Client.LogsSQL.
var logColumns = []string{"p_timestamp", "severity_text", "body", "service.name"}

// instructions is what a connected client receives before any tool call: the
// contract, the limits, and the three things that are easy to get wrong.
func instructions(opts Options, catalog []Tool) string {
	var allowedWrites, refusedWrites []string
	for _, t := range catalog {
		if t.Tier == ReadOnly {
			continue
		}
		if allowed(opts, t) {
			allowedWrites = append(allowedWrites, t.Name)
			continue
		}
		refusedWrites = append(refusedWrites, t.Name)
	}
	sort.Strings(allowedWrites)
	sort.Strings(refusedWrites)

	writeLine := "No writes are permitted on this endpoint: every write tool answers manual_action_required."
	if len(allowedWrites) > 0 {
		writeLine = fmt.Sprintf("The only permitted writes are %s.", strings.Join(allowedWrites, ", "))
	}

	var b strings.Builder
	b.WriteString("pmcluster control plane, served by the daemon itself at /api/mcp under the same bearer token as its REST API. ")
	b.WriteString("Role and stack scope are enforced by the daemon per call, not by this list.\n\n")

	b.WriteString("READS are all served. ")
	b.WriteString(writeLine + " ")
	b.WriteString("Every other write — and every dangerous operation (container exec, secret reveal, deletions, TLS, backup restore) — answers a result with ")
	b.WriteString(`"status":"manual_action_required"`)
	b.WriteString(", the exact `api` call, a `command` when one exists, and `do_not_retry:true`. ")
	b.WriteString("Relay that to the user verbatim; do not retry it, and do not look for another tool that performs the same change.\n\n")

	b.WriteString("Deploys are asynchronous: a deploy answers 202 with a revision and applies in the background. ")
	b.WriteString("Poll pmc_get_stack (last_error, failover) instead of assuming the work finished.\n\n")

	b.WriteString("OBSERVABILITY IS A BOUNDED VIEW, not the raw store:\n")
	b.WriteString("- log rows carry only " + strings.Join(logColumns, ", ") + "; pages are capped at ")
	fmt.Fprintf(&b, "%d rows, offset at most %d.\n", observability.MaxLogPageSize, observability.MaxLogOffset)
	b.WriteString("- node metrics expose " + MetricList("node") + "; container metrics expose " + MetricList("container") + ".\n")
	b.WriteString("- system.cpu.utilization is the IDLE fraction (state=idle): CPU busy = 1 - value.\n")
	fmt.Fprintf(&b, "- traces are aggregated: %d per page, %d spans per trace.\n", observability.MaxTracePageSize, observability.MaxTraceSpans)
	b.WriteString("- historical depth lives in Parseable, which requires its own credentials; this API cannot reach it for you.\n\n")

	b.WriteString("RESOURCES carry the schemas, so prefer reading one over probing with tool calls: ")
	b.WriteString("pmcluster://schema/api, pmcluster://schema/logs, pmcluster://schema/metrics, pmcluster://policy.")
	return b.String()
}

// MetricList renders the metric names one view exposes, from the observability
// package's constants.
func MetricList(view string) string {
	if view == "container" {
		return strings.Join([]string{
			observability.MetricContainerCPU,
			observability.MetricContainerMemoryPercent,
		}, ", ")
	}
	return strings.Join([]string{
		observability.MetricNodeCPUUtilization,
		observability.MetricNodeMemUtilization,
		observability.MetricNodeLoad1m,
	}, ", ")
}

// resourceDoc is one published resource and its body.
type resourceDoc struct {
	res  mcp.Resource
	body string
}

// resources returns the schema documents the server publishes.
func resources(opts Options, catalog []Tool, spec []byte) []resourceDoc {
	doc := func(uri, name, desc, body string) resourceDoc {
		return resourceDoc{
			res:  mcp.Resource{URI: uri, Name: name, Description: desc, MIMEType: "text/markdown"},
			body: body,
		}
	}
	return []resourceDoc{
		doc("pmcluster://schema/api", "api-schema",
			"The full OpenAPI description the tool catalog is derived from: every operation, request body and response schema.",
			string(spec)),
		doc("pmcluster://schema/logs", "logs-schema",
			"Exactly which fields log rows carry, the filters that exist, and the paging limits.",
			logsSchema()),
		doc("pmcluster://schema/metrics", "metrics-schema",
			"Every metric name this API exposes, per view, with units and the idle-fraction caveat.",
			metricsSchema()),
		doc("pmcluster://policy", "write-policy",
			"Which writes this endpoint permits, which it refuses, and the refusal shape.",
			policyDoc(opts, catalog)),
	}
}

func logsSchema() string {
	var b strings.Builder
	b.WriteString("# Log rows\n\n")
	b.WriteString("Source: pmcluster-logs stream in Parseable, projected to these columns only:\n\n")
	for _, c := range logColumns {
		b.WriteString("- `" + c + "`\n")
	}
	b.WriteString("\nNot available here: trace/span ids, host and container labels, resource attributes, or any custom field. ")
	b.WriteString("For those, query Parseable directly with its own credentials.\n\n")
	b.WriteString("Filters: window (`start`+`end` RFC3339, or `range`/`window` shorthand), `service` (exact `service.name`), ")
	b.WriteString("`severity` (error|warn|info|debug), `search` free text.\n\n")
	fmt.Fprintf(&b, "Paging: `limit` ≤ %d, `offset` ≤ %d. Ordering is oldest-first; `truncated` marks a cut page.\n\n",
		observability.MaxLogPageSize, observability.MaxLogOffset)
	b.WriteString("Values pass through a redactor: keys such as password, secret, token, authorization and api_key, plus URL credentials, are replaced with `<REDACTED>`.\n")
	b.WriteString("\nDerived counts: `pmc_observability_log_volume` returns per-bucket counts by level; `pmc_observability_service_names` lists the service identities seen.\n")
	return b.String()
}

func metricsSchema() string {
	var b strings.Builder
	b.WriteString("# Metrics\n\n")
	b.WriteString("## Node view (`pmc_observability_node_metrics`)\n\n")
	b.WriteString("| metric | states | meaning |\n|---|---|---|\n")
	b.WriteString("| `" + observability.MetricNodeCPUUtilization + "` | `idle` | **idle fraction** — CPU busy = 1 − value |\n")
	b.WriteString("| `" + observability.MetricNodeMemUtilization + "` | `used` | **used fraction** — memory used = value |\n")
	b.WriteString("| `" + observability.MetricNodeLoad1m + "` | — | 1-minute load average (counts runnable **and** IO-blocked tasks; not a CPU percentage) |\n\n")
	b.WriteString("## Container view (`pmc_observability_service_metrics`)\n\n")
	b.WriteString("| metric | meaning |\n|---|---|\n")
	b.WriteString("| `" + observability.MetricContainerCPU + "` | container CPU utilization fraction |\n")
	b.WriteString("| `" + observability.MetricContainerMemoryPercent + "` | container memory as a percentage |\n\n")
	b.WriteString("Both views return per-bucket AVG/MAX aggregates, not raw data points: node rows are grouped by host+metric+state, ")
	fmt.Fprintf(&b, "container rows by stack+service+container (≤ %d rows, highest peak first).\n", observability.MaxServiceMetricRows)
	b.WriteString("`bucket` is a duration (\"1m\", \"5m\"); when omitted it is derived from the window. Any other metric name in the ")
	b.WriteString("stream has no endpoint here — that is Parseable territory.\n")
	return b.String()
}

func policyDoc(opts Options, catalog []Tool) string {
	var allowedWrites, refused, dangerous []string
	for _, t := range catalog {
		switch {
		case t.Tier == ReadOnly:
			continue
		case t.Tier == Dangerous:
			dangerous = append(dangerous, t.Name)
		case allowed(opts, t):
			allowedWrites = append(allowedWrites, t.Name)
		default:
			refused = append(refused, t.Name)
		}
	}
	sort.Strings(allowedWrites)
	sort.Strings(refused)
	sort.Strings(dangerous)

	var b strings.Builder
	b.WriteString("# Write policy of this endpoint\n\n")
	b.WriteString("Reads: all " + fmt.Sprint(countReads(catalog)) + " read tools are served.\n\n")
	b.WriteString("Permitted writes (" + fmt.Sprint(len(allowedWrites)) + "):\n")
	if len(allowedWrites) == 0 {
		b.WriteString("- none\n")
	}
	for _, n := range allowedWrites {
		b.WriteString("- `" + n + "`\n")
	}
	if len(refused) > 0 {
		b.WriteString("\nRefused writes (" + fmt.Sprint(len(refused)) + ") — answered with manual_action_required, never performed:\n")
		for _, n := range refused {
			b.WriteString("- `" + n + "`\n")
		}
	}
	if len(dangerous) > 0 {
		b.WriteString("\nNever served (" + fmt.Sprint(len(dangerous)) + "), whatever the operator sets:\n")
		for _, n := range dangerous {
			b.WriteString("- `" + n + "`\n")
		}
	}
	b.WriteString("\nA config update is additionally checked against the target's own scope: cluster-scope configs are refused.\n\n")
	b.WriteString("Refusal shape:\n\n```json\n")
	example, _ := json.MarshalIndent(manualActionPayload{
		Tool:        "pmc_deploy",
		Reason:      "pmc_deploy is a write operation that is not on this MCP endpoint's allowlist",
		Command:     "pmcluster deploy <manifest>",
		API:         "curl -sS -X POST https://<daemon>/api/stacks -H \"Authorization: Bearer $PMC_TOKEN\" -H 'Content-Type: application/json' -d @body.json",
		Where:       "on a cluster node with the data directory or a leader's shell",
		TellTheUser: "I cannot make this change myself: it is not permitted on this MCP endpoint.",
		DoNotRetry:  true,
	}, "", "  ")
	b.WriteString(string(example))
	b.WriteString("\n```\n")
	return b.String()
}

func countReads(catalog []Tool) int {
	n := 0
	for _, t := range catalog {
		if t.Tier == ReadOnly {
			n++
		}
	}
	return n
}

// addResources publishes the schema documents on a server.
func addResources(srv *mcp.Server, opts Options, catalog []Tool, spec []byte) {
	for _, r := range resources(opts, catalog, spec) {
		resource, body := r.res, r.body
		srv.AddResource(&resource, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
				URI:      req.Params.URI,
				MIMEType: resource.MIMEType,
				Text:     body,
			}}}, nil
		})
	}
}
