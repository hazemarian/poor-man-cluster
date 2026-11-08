package api

import (
	"net/http"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodestat"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// NodeStatsHandler returns the latest per-node sample from node_stats.
//
// This is the console's overview data source and it reads the CONTROL-PLANE DB,
// never Parseable: after the 2026-11-08 outage the panel had to answer "what is
// eating this node" while the telemetry pipeline was the thing that was broken.
//
// One row per node, always. The rows are upserted by the leader's daemon from
// the collector's JSONL file; this handler only renders them.
func NodeStatsHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if st == nil {
			writeJSON(w, http.StatusOK, map[string]any{"node_stats": []any{}})
			return
		}
		rows, err := st.ListNodeStats(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			out = append(out, map[string]any{
				"hostname":     row.Hostname,
				"role":         row.Role,
				"cores":        row.Cores,
				"load1":        row.Load1,
				"load5":        row.Load5,
				"load15":       row.Load15,
				"mem_total_mb": row.MemTotalMB,
				"mem_used_mb":  row.MemUsedMB,
				"mem_avail_mb": row.MemAvailMB,
				"docker_up":    row.DockerUp != 0,
				"top_rss":      nodeStatProcs(row.TopRSS),
				"top_cpu":      nodeStatProcs(row.TopCPU),
				"sample_ts":    row.SampleTS,
				"updated_at":   row.UpdatedAt,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"node_stats": out})
	}
}

// nodeStatProcs renders the capped top lists as JSON objects. The cap is
// enforced at the sample source (nodestat.TopN); this only shapes them.
func nodeStatProcs(procs []nodestat.Proc) []map[string]any {
	out := make([]map[string]any, 0, len(procs))
	for _, p := range procs {
		out = append(out, map[string]any{
			"pid":     p.PID,
			"name":    p.Name,
			"rss_mb":  p.RSSMB,
			"cpu_pct": p.CPUPct,
		})
	}
	return out
}
