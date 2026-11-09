package controllers

import (
	"fmt"
	"os"
	"testing"
)

// TestPreviewDump writes the rendered panel to a file for a manual visual
// check. Temporary scaffolding, not part of the suite's assertions.
func TestPreviewDump(t *testing.T) {
	path := os.Getenv("NODESTAT_PREVIEW_OUT")
	if path == "" {
		t.Skip("NODESTAT_PREVIEW_OUT not set")
	}
	leader := []string{
		procJSON(41120, "parseable", 439, 1.1),
		procJSON(41121, "docker", 252, 0.4),
		procJSON(41122, "otelcol-contrib", 118, 1.1),
		procJSON(41123, "parseable", 231, 0.6),
		procJSON(41124, "fail2ban-server", 49, 0.1),
	}
	cpu := []string{
		procJSON(41122, "otelcol-contrib", 118, 11.1),
		procJSON(41120, "parseable", 439, 6.3),
		procJSON(41121, "docker", 252, 2.4),
		procJSON(41125, "next-server", 92, 1.8),
		procJSON(41126, "fail2ban-server", 49, 0.4),
	}
	nodeStatsFixture = nodeStatsBody(
		nodeStatWithProcs("nxt-sw-5-m", "manager", leader, cpu),
		nodeStatWithProcs("nxt-sw-4-m", "manager", leader[:2], cpu[:2]),
		nodeStatWithProcs("nxt-sw-3-w", "worker", leader[:1], cpu[:1]),
		nodeStatWithProcs("nxt-sw-1-m", "manager", leader[:3], cpu[:3]),
	)
	h := newCtrlHarness(t)
	h.mountOverview()
	body := h.get(t, "/web/overview").Body.String()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	fmt.Println("wrote", path)
}
