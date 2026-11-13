package cli

// The follower side of the cluster-state channel (slice 2 of
// docs/adr/0003-cluster-state-by-pull.md): a manager that is NOT the leader pulls the
// leader's snapshot and applies it to its own store, so the settings it answers with
// are the cluster's real settings instead of whatever the last control-plane kit
// happened to carry.
//
// Three rules this file exists to enforce, each of which was a bug somewhere else
// first:
//
//  1. The loop starts BEFORE the leadership wait — a follower manager blocks there
//     forever — and stops before the leader's own store is opened, because at that
//     moment this node becomes the writer and two writers to one row is not a
//     parallel run, it is a race whose winner nobody can predict.
//  2. The leader never fetches from itself. It reads its own store; mirroring would
//     be a second path applying a different answer to the same rows.
//  3. A poll that fails changes NOTHING. The last applied snapshot stays, and the log
//     says so — a mirror that empties itself on a transport error is how a failover
//     turns into a blank console.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/clustersnapshot"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runners"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// mirrorDeps is the loop's whole surface. Every field is a seam a test drives: no
// docker, no TLS and no clock leaks into the tick logic.
type mirrorDeps struct {
	// Store opens this node's control-plane database. A function because the loop
	// opens it lazily (a follower mid-`join` has no data directory yet) and retries
	// every tick rather than deciding once at boot.
	Store func() (*store.Store, error)
	// Fetch performs one conditional GET. See clustersnapshot.Client.
	Fetch func(ctx context.Context, ifNoneMatch string) (clustersnapshot.Result, error)
	// Leads reports whether THIS node is the swarm leader right now. Answered per
	// tick, not once at start: promotion can happen while the loop is running.
	Leads func(ctx context.Context) bool
	// Now is the clock that stamps applied_at and mirrored_at (this node's, never the
	// leader's).
	Now func() time.Time
	// Interval overrides the poll cadence (tests).
	Interval time.Duration
	Log      zerolog.Logger
}

func (d mirrorDeps) interval() time.Duration {
	if d.Interval > 0 {
		return d.Interval
	}
	return clustersnapshot.PollInterval
}

func (d mirrorDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// mirrorTick is one poll, split out of the loop so a test drives exactly one.
//
// It is deliberately total: every outcome (applied, unchanged, refused, leader) ends
// in a log line, because a mirror that says nothing is indistinguishable from a mirror
// that is working.
func mirrorTick(ctx context.Context, d mirrorDeps, st *store.Store) error {
	if d.Leads(ctx) {
		// Belt and braces: serve.go stops this loop the moment the leadership wait
		// returns, so this branch covers the window where a promotion lands mid-poll.
		d.Log.Debug().Msg("cluster mirror: this node leads; the store is the source, nothing to pull")
		return nil
	}

	// The ETag is the (leader, version) pair this node last APPLIED. Sending a bare
	// version would match another leader's identical number after a failover and
	// answer 304 for state that was never applied here.
	ifNoneMatch := ""
	prev, err := st.SyncState(ctx)
	switch {
	case err == nil:
		ifNoneMatch = clustersnapshot.ETag(prev.Leader, prev.Version)
	case errors.Is(err, store.ErrNeverSynced):
		// First poll on this node: an unconditional read.
	default:
		return err
	}

	res, err := d.Fetch(ctx, ifNoneMatch)
	if err != nil {
		return err
	}
	if res.NotModified {
		d.Log.Debug().Int64("version", prev.Version).Str("leader", prev.Leader).
			Msg("cluster mirror: already at the leader's version (304)")
		return nil
	}
	if res.Snapshot == nil {
		return errors.New("cluster mirror: the leader answered 200 with no snapshot")
	}

	appliedAt := d.now().Unix()
	if err := st.ApplySnapshot(ctx, res.Snapshot, res.Leader, appliedAt); err != nil {
		return err
	}
	d.Log.Info().
		Str("leader", res.Leader).
		Int64("version", res.Snapshot.Version).
		Int64("generated_at", res.Snapshot.GeneratedAt).
		Int("settings", len(res.Snapshot.Settings)).
		Int("secrets", len(res.Snapshot.Secrets)).
		Int("users", len(res.Snapshot.Users)).
		Msg("cluster mirror: applied the leader's snapshot")
	return nil
}

// snapshotMirrorFor assembles the loop from a running daemon's parts.
//
// The transport is the node-facing client the runner assignment already uses: same
// certificate, same CA checks, same candidate manager walk. Only the path differs.
func snapshotMirrorFor(cfg *config.Config, dc runtime.Client, hostname string, log zerolog.Logger) mirrorDeps {
	componentLog := log.With().Str("component", "cluster-mirror").Logger()
	client := &clustersnapshot.Client{
		Do: &runners.Client{
			Managers: func(ctx context.Context) ([]string, error) { return managerHosts(ctx, dc) },
			Log:      componentLog,
		},
		Log: componentLog,
	}
	return mirrorDeps{
		// Its own handle, opened lazily: on a follower the daemon holds no store at all
		// (a standby must not serve a stale one), and this loop is the only writer.
		Store: func() (*store.Store, error) { return openExistingStore(cfg.DBPath()) },
		Fetch: client.Fetch,
		Leads: func(ctx context.Context) bool {
			leader, _, err := isSwarmLeader(ctx, dc, hostname)
			return err == nil && leader
		},
		Log: componentLog,
	}
}

// openExistingStore opens the control-plane database ONLY if it already exists. The
// mirror loop and the CLI's mirror-labelled reads both use it, so neither can seed the
// database the other refuses to create.
//
// store.Open creates a database (and its directory) when the file is missing, and on a
// follower that would be a lie: an empty data.db is exactly what serve.go's "data
// directory not initialised — run pmcluster init" guard exists to catch, and creating it
// here would make the guard pass on a node that was never initialised. A follower
// mid-join simply retries next interval.
//
// A failure is never fatal for the same reason: the loop retries every interval, which is
// also the answer to the one race this can lose — the migration runner is not
// concurrency-safe across processes (0001 is a plain CREATE TABLE), so an opener that
// starts while another process is mid-migration fails once and then finds the work
// already done.
func openExistingStore(dbPath string) (*store.Store, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("no control-plane database at %s yet: %w", dbPath, err)
	}
	return store.Open(dbPath)
}

// startSnapshotMirror runs the poll loop until stop is called (or ctx ends), and
// returns a done channel that closes only after the loop has finished its last tick
// and closed its store handle.
//
// Callers MUST wait on done before opening their own handle on data.db: that ordering
// is what keeps a single writer.
func startSnapshotMirror(ctx context.Context, d mirrorDeps) (stop func(), done <-chan struct{}) {
	runCtx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		var st *store.Store
		defer func() {
			if st != nil {
				_ = st.Close()
			}
		}()
		t := time.NewTicker(d.interval())
		defer t.Stop()
		for {
			if runCtx.Err() != nil {
				return
			}
			if st == nil {
				opened, err := d.Store()
				if err != nil {
					// Not fatal and not silent: a node that never gets a store will never
					// mirror, and the reason has to be in the log.
					d.Log.Warn().Err(err).
						Msg("cluster mirror: cannot open this node's store yet; not mirroring (retrying next interval)")
				} else {
					st = opened
				}
			}
			if st != nil {
				if err := mirrorTick(runCtx, d, st); err != nil && runCtx.Err() == nil {
					// The last applied snapshot stays in place: a failed poll must not
					// blank a mirror.
					d.Log.Warn().Err(err).
						Msg("cluster mirror: poll failed; still serving the last applied snapshot")
				}
			}
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return cancel, finished
}
