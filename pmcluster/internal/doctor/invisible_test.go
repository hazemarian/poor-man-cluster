package doctor

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// telemetryDocker serves one inspected service with whatever env the test wants.
type telemetryDocker struct {
	runtime.Client
	env     []string
	name    string
	missing bool
}

func (d telemetryDocker) ServiceInspect(context.Context, string) (runtime.ServiceInspectResult, error) {
	if d.missing {
		return runtime.ServiceInspectResult{}, context.DeadlineExceeded
	}
	return runtime.ServiceInspectResult{Name: d.name, Env: d.env}, nil
}

// TestCheckConsoleTelemetry is the check for the invisible component: the collector does not scrape
// the edge stack, so a console without an OTLP endpoint has no logs and no spans anywhere, and
// nothing else in the platform notices.
func TestCheckConsoleTelemetry(t *testing.T) {
	ok := checkConsoleTelemetry(context.Background(), Input{Docker: telemetryDocker{
		name: "edge_pmcluster-edge",
		env:  []string{"LISTEN_ADDR=:8042", "OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318"},
	}})
	if ok.Status != StatusOK || !strings.Contains(ok.Detail, "otel-collector:4318") {
		t.Errorf("with an endpoint: %+v", ok)
	}

	bad := checkConsoleTelemetry(context.Background(), Input{Docker: telemetryDocker{
		name: "edge_pmcluster-edge",
		env:  []string{"LISTEN_ADDR=:8042", "OTEL_EXPORTER_OTLP_ENDPOINT="},
	}})
	if bad.Status != StatusFail {
		t.Fatalf("an empty endpoint is not a failure: %+v", bad)
	}
	if !strings.Contains(bad.Detail, "reach nowhere") {
		t.Errorf("the failure does not say what it costs: %q", bad.Detail)
	}

	absent := checkConsoleTelemetry(context.Background(), Input{Docker: telemetryDocker{missing: true}})
	if absent.Status != StatusSkip {
		t.Errorf("no console in this swarm should SKIP, not fail: %+v", absent)
	}
}

// shellDocker serves a service list with shell labels.
type shellDocker struct {
	runtime.Client
	svcs []runtime.Service
}

func (d shellDocker) ServiceList(context.Context) ([]runtime.Service, error) { return d.svcs, nil }

// TestCheckShellSessions — a session outliving its ceiling means the socket close failed AND the
// five-minute sweep failed, and a leftover container has the stack's secrets mounted.
func TestCheckShellSessions(t *testing.T) {
	now := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	fresh := func(min int) runtime.Service {
		return runtime.Service{
			Name: "pmshell-aarma",
			Labels: map[string]string{
				"pmcluster.shell":         "true",
				"pmcluster.shell.created": strconvUnix(now.Add(-time.Duration(min) * time.Minute)),
			},
		}
	}

	ok := checkShellSessions(context.Background(), Input{
		Now: now, Docker: shellDocker{svcs: []runtime.Service{fresh(5)}},
	})
	if ok.Status != StatusOK {
		t.Errorf("a 5-minute-old session is not stale: %+v", ok)
	}

	stale := checkShellSessions(context.Background(), Input{
		Now: now, Docker: shellDocker{svcs: []runtime.Service{fresh(90)}},
	})
	if stale.Status != StatusWarn {
		t.Fatalf("a 90-minute-old session is not reported: %+v", stale)
	}
	if !strings.Contains(stale.Detail, "pmshell-aarma") {
		t.Errorf("the stale session is not named: %q", stale.Detail)
	}

	// A stack service with no shell label is never this check's business.
	plain := checkShellSessions(context.Background(), Input{
		Now: now, Docker: shellDocker{svcs: []runtime.Service{{Name: "aarma_web"}}},
	})
	if plain.Status != StatusOK {
		t.Errorf("an ordinary service was reported as a shell: %+v", plain)
	}
}

func strconvUnix(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}
