package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/clustersnapshot"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// MirrorStaleAfter is how old this node's last successful answer from the leader may be
// before it refuses to lead from what it has. Three poll intervals, the same number
// `doctor` uses for its STALE label — one threshold, one meaning.
const MirrorStaleAfter = 3 * clustersnapshot.PollInterval

// Adoption is what one leadership-gain check found.
type Adoption struct {
	// Ready is true when this node may serve as the writer: its store is either the
	// cluster's own state (a node that has never been a follower) or a mirror confirmed
	// recently enough to vouch for.
	Ready bool
	// Leader is the leader whose state this node mirrored ("" when it never mirrored).
	Leader string
	// Version is the mirrored version, 0 when nothing was ever applied.
	Version int64
	// Age is how long ago this node last got a successful answer from that leader, on
	// THIS node's clock (0 when unknown).
	Age time.Duration
	// CheckedAt is when that answer arrived.
	CheckedAt time.Time
	// Source reports that this node IS the cluster's state (it has never applied a
	// snapshot), so nothing was mirrored onto it.
	Source bool
	// KeyOK / KeyProblem report the encryption-key comparison. Neither is a refusal.
	KeyOK      bool
	KeyProblem string
	// Reason names the fact that decided the answer, for the log.
	Reason string
}

// RefusalError is returned when this node's mirror cannot be vouched for. It carries the
// numbers an operator needs to act: how old the mirror is and which leader it came from.
type RefusalError struct {
	Age    time.Duration
	Leader string
	Why    string
}

func (e *RefusalError) Error() string {
	switch {
	case e.Leader == "" && e.Age == 0:
		return "refusing leadership: " + e.Why
	case e.Leader == "":
		return fmt.Sprintf("refusing leadership: %s (mirror last confirmed %s ago)", e.Why, e.Age.Truncate(time.Second))
	default:
		return fmt.Sprintf("refusing leadership: %s (mirror from %s last confirmed %s ago)", e.Why, e.Leader, e.Age.Truncate(time.Second))
	}
}

// Now is the clock the age is measured against. Overridable in tests; production takes
// this node's clock and never the leader's stamp, because subtracting a remote timestamp
// from a local one is the cache-report bug.
var Now = time.Now

// AdoptOnLeadership decides whether this node may act as the cluster's writer after a
// leadership gain, using the store it ALREADY HAS.
//
// It copies nothing. That is the whole change: every manager mirrors the cluster's state
// continuously (internal/noderuntime/snapshot_mirror.go), so a promoted node is already
// holding it, and the old path — stage a kit out of a Raft config, close the store, rename
// a data.db and an .encryption_key into place, reopen — existed only because the mirror was
// a projection that could not be served from. See ADR 0005.
//
// The check is about TRUST in the rows the node is about to answer for, and the operator
// settled where the line sits:
//
//   - A STALE mirror is SERVED. A node whose snapshot is a few minutes old is promoted and
//     keeps serving it; refusing would turn a failover into an outage over a number the
//     operator decided is acceptable. The cost is made visible instead of hidden: the age
//     and the version are logged as the adoption happens ("adopting mirror version N, age
//     Ts"), so a leader that took over on old data says so once and forever in the journal.
//   - The ONE hard refusal is "nothing to serve": a node that has never applied a snapshot
//     AND whose store holds no cluster state (no setting, user, stack, secret or config) is
//     an empty control plane. That is not a stale copy, it is no copy, and a leader answering
//     from it would report a cluster with no users, no stacks and no settings as the truth.
//     The two cases must not share a code path — one is an age, the other is an absence.
//   - A node that has never applied a snapshot but DOES hold data is the cluster's source:
//     the first leader after `cluster up`, or a standalone box. It adopts itself.
//   - The encryption key is reported, not enforced here. A key that differs from the
//     cluster's means the secret ciphertext cannot be decrypted, and that is a loud WARN
//     (and a problem in `cluster mirror verify`), not a second way to end up with no leader.
//     The key still comes from its own config for now: the join-time key route is a separate
//     change.
//
// mirrored reports whether THIS process ran the mirror loop before the transition. It
// separates "never a follower" from "a follower whose polls never landed".
func (k *Kit) AdoptOnLeadership(ctx context.Context, st *store.Store, mirrored bool) (Adoption, error) {
	if k.Docker == nil {
		// Standalone/local: no swarm, no peer, no mirror. This node is its own source.
		return Adoption{Ready: true, Source: true, Reason: "standalone: this node is its own source"}, nil
	}
	if st == nil {
		return Adoption{}, errors.New("cannot verify this node's cluster state: no store handle")
	}
	log := k.Log.With().Str("component", "controlplane").Logger()

	adoption := Adoption{}
	state, err := st.SyncState(ctx)
	switch {
	case errors.Is(err, store.ErrNeverSynced):
		state = nil
	case err != nil:
		return Adoption{}, fmt.Errorf("read this node's mirror state: %w", err)
	}

	if state == nil {
		hasData, err := st.HasData(ctx)
		if err != nil {
			return Adoption{}, err
		}
		if !hasData {
			err := &RefusalError{Why: "no snapshot has ever been applied here and this node's store holds no cluster state — there is nothing to serve, which is not the same as serving something old"}
			log.Error().Err(err).Bool("mirrored", mirrored).
				Msg("control-plane adoption REFUSED on leadership gain: this node has nothing to serve")
			return adoption, err
		}
		adoption.Ready = true
		adoption.Source = true
		adoption.Reason = "this node holds the cluster's own state (no snapshot has ever been applied here)"
		log.Info().Bool("mirrored", mirrored).
			Msg("adopting this node's own store: it is the cluster's source, nothing to mirror")
	} else {
		adoption.Leader = state.Leader
		adoption.Version = state.Version
		age := Now().Sub(time.Unix(state.CheckedAt, 0))
		if age < 0 {
			// A backwards clock must not read as "fresh in the future".
			age = 0
		}
		adoption.Age = age
		adoption.CheckedAt = time.Unix(state.CheckedAt, 0)
		adoption.Ready = true
		adoption.Reason = fmt.Sprintf("serving mirror version %d from %s, age %s", state.Version, state.Leader, age.Truncate(time.Second))
		if age > MirrorStaleAfter {
			// Accepted, and said out loud. This is the operator's trade: a promotion on a
			// few-minutes-old snapshot beats no leader, and the journal carries the cost.
			log.Warn().Str("leader", state.Leader).Int64("version", state.Version).
				Dur("age", age).Dur("stale-after", MirrorStaleAfter).
				Msg("adopting the mirror anyway: it is older than three polls — serving it is accepted, but writes made elsewhere in that window are not here")
		} else {
			log.Info().Str("leader", state.Leader).Int64("version", state.Version).
				Dur("age", age).
				Msg("adopting mirror version " + strconv.FormatInt(state.Version, 10) + ", age " + age.Truncate(time.Second).String())
		}
	}

	if err := k.checkClusterKey(ctx); err != nil {
		// A warning, never a refusal: the key route is the next decision, and ending up
		// with no leader over a key the operator is about to replace is the wrong trade.
		adoption.KeyProblem = err.Error()
		log.Warn().Err(err).
			Msg("this node's encryption key is not the cluster's: secret ciphertext it mirrors cannot be decrypted until the key route lands")
	} else {
		adoption.KeyOK = true
	}

	log.Info().
		Bool("ready", adoption.Ready).
		Str("reason", adoption.Reason).
		Str("leader", adoption.Leader).
		Int64("version", adoption.Version).
		Dur("age", adoption.Age).
		Msg("this node is the cluster's writer, serving the store it already holds")
	return adoption, nil
}

// checkClusterKey proves this node holds the key the cluster's ciphertext was written with.
//
// The key still lives in its own Docker config (`pmcluster_state_key_*`) — the security
// split from ADR 0003 — and this reads it rather than adopting a file: the join-time key
// route is the next decision, not this one. A cluster that has never published a key config
// has nothing to disagree with, and a swarm that cannot be read right now is not evidence of
// a wrong key; both answer nil.
func (k *Kit) checkClusterKey(ctx context.Context) error {
	name, published, err := k.newestKey(ctx)
	switch {
	case errors.Is(err, ErrNoSnapshots):
		return nil
	case err != nil:
		if IsSwarmUnavailable(err) {
			return nil
		}
		return fmt.Errorf("read the cluster's key config: %w", err)
	}
	local, err := os.ReadFile(k.KeyPath())
	if err != nil {
		return fmt.Errorf("no local encryption key at %s: the mirrored secret ciphertext cannot be decrypted without it", k.KeyPath())
	}
	if !bytes.Equal(local, published) {
		return fmt.Errorf("the local encryption key differs from %s", name)
	}
	return nil
}
