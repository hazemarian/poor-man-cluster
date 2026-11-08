package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodestat"
)

// NodeStat is one row of node_stats: the newest sample for one node plus when
// the leader wrote it.
//
// The table holds exactly one row per node and is rewritten in place. That is a
// deliberate size bound: a 4-node 2GB box must not grow a metrics history in
// the control-plane SQLite file, which is the store every request reads from.
type NodeStat struct {
	nodestat.Stat
	UpdatedAt int64
}

// UpsertNodeStats writes one row per node, replacing the previous sample. Rows
// are never appended: a second call for the same hostname updates it.
func (s *Store) UpsertNodeStats(ctx context.Context, stats []nodestat.Stat, now int64) error {
	if len(stats) == 0 {
		return nil
	}
	db := s.conn()
	for _, st := range stats {
		if st.Hostname == "" {
			continue
		}
		rss, err := marshalProcs(st.TopRSS)
		if err != nil {
			return fmt.Errorf("marshal top rss for %s: %w", st.Hostname, err)
		}
		cpu, err := marshalProcs(st.TopCPU)
		if err != nil {
			return fmt.Errorf("marshal top cpu for %s: %w", st.Hostname, err)
		}
		_, err = db.ExecContext(ctx, `
			INSERT INTO node_stats (
				hostname, role, cores, load1, load5, load15,
				mem_total_mb, mem_used_mb, mem_avail_mb, docker_up,
				top_rss, top_cpu, sample_ts, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(hostname) DO UPDATE SET
				role         = excluded.role,
				cores        = excluded.cores,
				load1        = excluded.load1,
				load5        = excluded.load5,
				load15       = excluded.load15,
				mem_total_mb = excluded.mem_total_mb,
				mem_used_mb  = excluded.mem_used_mb,
				mem_avail_mb = excluded.mem_avail_mb,
				docker_up    = excluded.docker_up,
				top_rss      = excluded.top_rss,
				top_cpu      = excluded.top_cpu,
				sample_ts    = excluded.sample_ts,
				updated_at   = excluded.updated_at`,
			st.Hostname, st.Role, st.Cores, st.Load1, st.Load5, st.Load15,
			st.MemTotalMB, st.MemUsedMB, st.MemAvailMB, st.DockerUp,
			rss, cpu, st.SampleTS, now)
		if err != nil {
			return fmt.Errorf("upsert node stats %s: %w", st.Hostname, err)
		}
	}
	return nil
}

// ListNodeStats returns every recorded node row, ordered by hostname so the
// panel does not reshuffle between refreshes.
func (s *Store) ListNodeStats(ctx context.Context) ([]NodeStat, error) {
	rows, err := s.conn().QueryContext(ctx, `
		SELECT hostname, role, cores, load1, load5, load15,
		       mem_total_mb, mem_used_mb, mem_avail_mb, docker_up,
		       top_rss, top_cpu, sample_ts, updated_at
		FROM node_stats ORDER BY hostname`)
	if err != nil {
		return nil, fmt.Errorf("query node stats: %w", err)
	}
	defer rows.Close()

	out := make([]NodeStat, 0, 8)
	for rows.Next() {
		var (
			st              nodestat.Stat
			cores           sql.NullInt64
			load1, load5    sql.NullFloat64
			load15          sql.NullFloat64
			memTotal, memU  sql.NullInt64
			memAvail        sql.NullInt64
			dockerUp        sql.NullInt64
			rss, cpu        string
			sampleTS, updAt int64
		)
		if err := rows.Scan(
			&st.Hostname, &st.Role, &cores, &load1, &load5, &load15,
			&memTotal, &memU, &memAvail, &dockerUp,
			&rss, &cpu, &sampleTS, &updAt,
		); err != nil {
			return nil, fmt.Errorf("scan node stats: %w", err)
		}
		st.Cores = int(cores.Int64)
		st.Load1, st.Load5, st.Load15 = load1.Float64, load5.Float64, load15.Float64
		st.MemTotalMB, st.MemUsedMB, st.MemAvailMB = memTotal.Int64, memU.Int64, memAvail.Int64
		st.DockerUp = int(dockerUp.Int64)
		st.SampleTS = sampleTS
		if st.TopRSS, err = unmarshalProcs(rss); err != nil {
			return nil, fmt.Errorf("decode top rss for %s: %w", st.Hostname, err)
		}
		if st.TopCPU, err = unmarshalProcs(cpu); err != nil {
			return nil, fmt.Errorf("decode top cpu for %s: %w", st.Hostname, err)
		}
		out = append(out, NodeStat{Stat: st, UpdatedAt: updAt})
	}
	return out, rows.Err()
}

// DeleteNodeStatsExcept prunes rows for nodes no longer in the swarm. An empty
// keep list deletes everything (a cluster whose node list is empty has no nodes
// to show), which is why callers must pass the node list they actually read.
func (s *Store) DeleteNodeStatsExcept(ctx context.Context, keep []string) (int64, error) {
	db := s.conn()
	if len(keep) == 0 {
		res, err := db.ExecContext(ctx, `DELETE FROM node_stats`)
		if err != nil {
			return 0, fmt.Errorf("delete node stats: %w", err)
		}
		return res.RowsAffected()
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",")
	args := make([]any, 0, len(keep))
	for _, h := range keep {
		args = append(args, h)
	}
	res, err := db.ExecContext(ctx, `DELETE FROM node_stats WHERE hostname NOT IN (`+placeholders+`)`, args...)
	if err != nil {
		return 0, fmt.Errorf("prune node stats: %w", err)
	}
	return res.RowsAffected()
}

func marshalProcs(procs []nodestat.Proc) (string, error) {
	if len(procs) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(procs)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func unmarshalProcs(s string) ([]nodestat.Proc, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []nodestat.Proc
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
}
