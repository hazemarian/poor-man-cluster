package services

import (
	"testing"

	"github.com/docker/docker/api/types/container"
)

func TestAggregateStandalone_ComposeReplicasAndStoppedPredecessors(t *testing.T) {
	rows := aggregateStandalone([]container.Summary{
		{
			ID:      "web-1",
			Names:   []string{"/demo-web-1"},
			Labels:  map[string]string{"com.docker.compose.project": "demo", "com.docker.compose.service": "web"},
			State:   container.StateRunning,
			Image:   "example/web:latest",
			Created: 20,
		},
		{
			ID:      "web-2-old",
			Names:   []string{"/demo-web-2"},
			Labels:  map[string]string{"com.docker.compose.project": "demo", "com.docker.compose.service": "web"},
			State:   container.StateExited,
			Status:  "Exited (1) 2 minutes ago",
			Created: 30,
		},
	})

	if len(rows) != 1 {
		t.Fatalf("aggregate returned %d rows, want one canonical service", len(rows))
	}
	row := rows[0]
	if row.Name != "demo_web" || row.Stack != "demo" {
		t.Fatalf("identity = %q/%q, want demo/web", row.Name, row.Stack)
	}
	if row.Replicas != 1 {
		t.Errorf("observed replicas = %d, want 1", row.Replicas)
	}
	if row.DesiredKnown || row.Desired != 0 {
		t.Errorf("desired state = known:%v count:%d, want unknown/0", row.DesiredKnown, row.Desired)
	}
	if row.UpdateState != "running" || row.UpdateError != "" {
		t.Errorf("aggregate state = %q/%q, want running/empty", row.UpdateState, row.UpdateError)
	}
}

func TestAggregateStandalone_UnlabeledContainersUseStableIdentity(t *testing.T) {
	rows := aggregateStandalone([]container.Summary{{
		Names: []string{"/worker"},
		State: container.StateRunning,
	}})
	if len(rows) != 1 {
		t.Fatalf("aggregate returned %d rows, want one", len(rows))
	}
	if rows[0].Name != "standalone_worker" || rows[0].Stack != "standalone" {
		t.Fatalf("identity = %q/%q, want standalone/worker", rows[0].Name, rows[0].Stack)
	}
	if !rows[0].Standalone || rows[0].DesiredKnown {
		t.Errorf("standalone flags = standalone:%v desired-known:%v, want true/false", rows[0].Standalone, rows[0].DesiredKnown)
	}
}

func TestStandaloneReadOnlyOperationsExplainTelemetryPath(t *testing.T) {
	var s Standalone
	if _, err := s.Tasks(nil, "demo", "web"); err == nil {
		t.Fatal("Tasks unexpectedly succeeded")
	} else if err.Error() != "standalone task history is unavailable; use telemetry evidence" {
		t.Fatalf("Tasks error = %q", err)
	}
	if _, err := s.Logs(nil, "demo", "web", 20); err == nil {
		t.Fatal("Logs unexpectedly succeeded")
	} else if err.Error() != "standalone service logs are available in telemetry evidence" {
		t.Fatalf("Logs error = %q", err)
	}
	if err := s.Restart(nil, "demo", "web"); err != errStandaloneReadOnly {
		t.Fatalf("Restart error = %v, want read-only sentinel", err)
	}
}
