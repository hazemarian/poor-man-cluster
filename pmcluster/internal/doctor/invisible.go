package doctor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// This file holds two checks that exist because each one's absence cost real time on
// this platform: a component that shipped no telemetry at all, and a session that
// outlived its socket. Orphan data dirs are already covered by checkUnbackedData.

// edgeServiceName is the console's swarm service. A constant, not a lookup: the
// console is platform-owned and its name comes from edge-stack.yml.
const edgeServiceName = "edge_pmcluster-edge"

// checkConsoleTelemetry answers "is the console shipping its own telemetry".
//
// The collector deliberately does NOT scrape the edge stack (its access-log volume was
// noise), so the console is only observable if its own OTLP export is wired — and it
// silently was not for months: the one component an operator stares at all day had no
// logs and no spans anywhere, and nothing said so. The endpoint is the whole
// precondition; with it set, the console emits.
func checkConsoleTelemetry(ctx context.Context, in Input) Check {
	const name = "console telemetry"
	if in.Docker == nil {
		return Check{Name: name, Status: StatusSkip, Detail: "no swarm client on this host"}
	}
	svc, err := in.Docker.ServiceInspect(ctx, edgeServiceName)
	if err != nil || svc.Name == "" {
		return Check{Name: name, Status: StatusSkip, Detail: "the console is not part of this swarm"}
	}
	for _, kv := range svc.Env {
		if v, ok := strings.CutPrefix(kv, "OTEL_EXPORTER_OTLP_ENDPOINT="); ok && strings.TrimSpace(v) != "" {
			return Check{Name: name, Status: StatusOK, Detail: "the console exports to " + strings.TrimSpace(v)}
		}
	}
	return Check{
		Name:   name,
		Status: StatusFail,
		Detail: "the console has no OTEL_EXPORTER_OTLP_ENDPOINT while the collector does not scrape the edge stack, so its logs and spans reach nowhere — set it in edge-stack.yml (http://otel-collector:4318, and the edge must join monitoring-net)",
	}
}

// checkShellSessions reports sessions that outlived their ceiling.
//
// A session is removed when its socket closes, and the daemon sweeps the rest every
// five minutes. A session older than the ceiling therefore means BOTH failed — and a
// leaked throwaway container with the stack's secrets mounted is not something to
// discover by accident.
func checkShellSessions(ctx context.Context, in Input) Check {
	const name = "shell sessions"
	if in.Docker == nil {
		return Check{Name: name, Status: StatusSkip, Detail: "no swarm client on this host"}
	}
	svcs, err := in.Docker.ServiceList(ctx)
	if err != nil {
		return Check{Name: name, Status: StatusSkip, Detail: "could not list services: " + err.Error()}
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	ceiling := runtime.ShellMaxLife + 2*time.Minute
	var stale []string
	for _, sv := range svcs {
		if sv.Labels["pmcluster.shell"] != "true" {
			continue
		}
		secs, perr := strconv.ParseInt(sv.Labels["pmcluster.shell.created"], 10, 64)
		if perr != nil {
			continue
		}
		if age := now.Sub(time.Unix(secs, 0)); age > ceiling {
			stale = append(stale, fmt.Sprintf("%s (%.0fm old)", sv.Name, age.Minutes()))
		}
	}
	if len(stale) == 0 {
		return Check{Name: name, Status: StatusOK, Detail: "no session outlived its ceiling"}
	}
	sort.Strings(stale)
	return Check{
		Name:   name,
		Status: StatusWarn,
		Detail: "a session outlived its ceiling and the sweeper has not collected it: " + strings.Join(stale, ", ") + " — the socket did not close cleanly AND the five-minute sweep did not run; check the daemon log for 'shell: the sweeper'",
	}
}
