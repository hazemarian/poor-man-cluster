package clustersnapshot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// VerifyReport is what "would a promotion work from THIS node" measured.
//
// It replaced the control-plane kit drill (ADR 0005): the kit is retired, and the thing
// whose health decides whether a promotion works is now the mirror. The failure this check
// exists for is unchanged — a node whose cluster state looks fine until it is the only node
// left — so the report answers the same three questions the kit drill answered, from the
// state the node would actually promote on: which version, how old, and does this node hold
// the key that decrypts it.
type VerifyReport struct {
	// Source is a sentence: "this node is the writer" or "mirror of <leader>".
	Source string `json:"source"`
	// Leader is the leader whose state this node mirrored ("" when it never mirrored).
	Leader string `json:"leader,omitempty"`
	// Version is the cluster_version applied here.
	Version int64 `json:"version"`
	// AppliedAt is when the rows were written on this node's clock; CheckedAt is when this
	// node last got a successful answer (200 or 304). The age is measured from CheckedAt.
	AppliedAt  int64 `json:"applied_at,omitempty"`
	CheckedAt  int64 `json:"checked_at,omitempty"`
	AgeSeconds int64 `json:"age_seconds"`
	// NeverApplied reports that no snapshot has ever landed here.
	NeverApplied bool `json:"never_applied"`
	// Tables is the row count per mirrored table, TotalRows their sum.
	Tables    map[string]int `json:"tables"`
	TotalRows int            `json:"total_rows"`
	// KeyPresent reports whether this node holds the encryption key.
	KeyPresent bool `json:"key_present"`
	// Warnings are facts an operator should know before a promotion: a stale mirror (which
	// is served by decision, see AdoptOnLeadership), a stack whose revision is missing.
	Warnings []string `json:"warnings,omitempty"`
	// Problems are the reasons a promotion would serve something broken.
	Problems []string `json:"problems,omitempty"`
}

// OK reports whether a promotion from this node would serve working cluster state.
func (r *VerifyReport) OK() bool { return len(r.Problems) == 0 }

// Verify measures this node's mirror: which leader's version it holds, how old the last
// successful answer is, how many rows every table carries, and whether a key is present.
//
// It reads only; nothing is extracted, restored or written. hasKey is the caller's answer to
// "is a cipher loaded" (the doctor has one, the CLI opens the key file), because the key is
// not part of the mirror and this package must not invent it.
func Verify(ctx context.Context, st *store.Store, hasKey bool, now time.Time) (*VerifyReport, error) {
	if st == nil {
		return nil, errors.New("no control-plane database on this node: there is no mirror to verify")
	}
	if now.IsZero() {
		now = time.Now()
	}
	rep := &VerifyReport{KeyPresent: hasKey}

	counts, err := st.TableRowCounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("count the mirrored tables: %w", err)
	}
	rep.Tables = counts
	for _, n := range counts {
		rep.TotalRows += n
	}

	state, err := st.SyncState(ctx)
	switch {
	case errors.Is(err, store.ErrNeverSynced):
		rep.NeverApplied = true
		rep.Source = "this node is the writer (no snapshot has ever been applied here)"
		// HasData, not a row count: cluster_version is seeded by the migration, so a
		// "rows > 0" test would call an otherwise empty store populated. The question is
		// whether there is a setting, a user, a stack, a secret or a config — cluster state
		// somebody would miss.
		hasData, derr := st.HasData(ctx)
		if derr != nil {
			return nil, derr
		}
		if !hasData {
			rep.Problems = append(rep.Problems,
				"this node holds no cluster state at all: a promotion from it would serve an empty cluster (no settings, no users, no stacks)")
		}
	case err != nil:
		return nil, fmt.Errorf("read this node's mirror state: %w", err)
	default:
		rep.Leader = state.Leader
		rep.Version = state.Version
		rep.AppliedAt = state.AppliedAt
		rep.CheckedAt = state.CheckedAt
		rep.Source = "mirror of " + state.Leader
		age := now.Sub(time.Unix(state.CheckedAt, 0))
		if age < 0 {
			age = 0
		}
		rep.AgeSeconds = int64(age / time.Second)
		if age > 3*PollInterval {
			// Accepted, not a problem: the operator decided a promotion on a few-minutes-old
			// snapshot beats no leader. Saying it here is what keeps the decision visible.
			rep.Warnings = append(rep.Warnings, fmt.Sprintf(
				"the mirror is %s old (the last successful answer from %s): a promotion would serve it — accepted, and logged at adoption",
				age.Truncate(time.Second), state.Leader))
		}
	}

	if !hasKey {
		// A WARNING, not a problem: state is verified, and the key is the part of the
		// control plane this node has never been promised (the join-time key route is the
		// next change). Failing the whole report on it would make every fresh node's
		// doctor red for a decision nobody has taken yet — but staying silent would hide
		// the one thing that makes every secret unreadable after a promotion.
		rep.Warnings = append(rep.Warnings,
			"no encryption key on this node: every secret this mirror carries is ciphertext it cannot decrypt (the join-time key route is not built yet)")
	}

	// The stack pointer is only useful with the revision behind it: a promoted leader that
	// knows stack X is on revision 42 but has no row 42 cannot reconcile or roll back until
	// somebody re-deploys. Mirrored now, so the check can be answered from here.
	if stacks, ok := counts["stacks"]; ok && stacks > 0 {
		if revs := counts["stack_revisions"]; revs == 0 {
			rep.Warnings = append(rep.Warnings,
				fmt.Sprintf("%d stack(s) and no stack revision at all: a promoted leader could not reconcile or roll back a deploy until each stack is redeployed", stacks))
		}
	}
	return rep, nil
}

// Human is the one-line summary every surface prints, so the CLI and the doctor say the
// same sentence about the same numbers.
func (r *VerifyReport) Human() string {
	who := r.Source
	if r.NeverApplied {
		return fmt.Sprintf("%s · version %d · %d row(s) in %d table(s)", who, r.Version, r.TotalRows, len(r.Tables))
	}
	return fmt.Sprintf("%s · version %d · %d row(s) in %d table(s) · confirmed %ds ago",
		who, r.Version, r.TotalRows, len(r.Tables), r.AgeSeconds)
}
