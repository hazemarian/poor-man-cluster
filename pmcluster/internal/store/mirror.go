package store

import (
	"context"
	"fmt"
	"time"
)

// MirrorReading is what a read surface needs in order to LABEL an answer taken from this
// node's mirror: whose state it is, how old it is, and how much of it there is.
//
// It exists because the alternative is what shipped before — a follower refusing to
// answer at all, or (worse) answering from a copy of unknown age as though it were
// current. A stale answer with its age attached is useful; an undated one is a lie, and
// a blank one leaves an operator unable to tell "nothing there" from "nobody asked".
type MirrorReading struct {
	Leader      string
	Version     int64
	GeneratedAt int64
	AppliedAt   int64
	// AgeSeconds is how long ago this node APPLIED the snapshot, measured on this node's
	// clock — never by subtracting the leader's generated_at from a local clock, which is
	// how a fresh report reads as stale the instant it is written. Clamped at zero: a
	// clock that steps backwards must not produce a negative age that reads as "the
	// future".
	AgeSeconds int64
	// Settings/Secrets/Users are how many rows the mirror holds. The count is what makes
	// the answer useful ("the mirror has the cluster's settings") instead of merely
	// plausible ("a version was recorded").
	Settings int
	Secrets  int
	Users    int
}

// MirrorReading reads the mirror's state and counts what it holds.
//
// Returns ErrNeverSynced on a node that has never applied a snapshot: a leader (which is
// the source, not a mirror), a worker (which has no store), or a follower that has not
// polled yet. Each caller says which of those it is.
func (s *Store) MirrorReading(ctx context.Context, now time.Time) (*MirrorReading, error) {
	state, err := s.SyncState(ctx)
	if err != nil {
		return nil, err
	}
	m := &MirrorReading{
		Leader:      state.Leader,
		Version:     state.Version,
		GeneratedAt: state.GeneratedAt,
		AppliedAt:   state.AppliedAt,
	}
	if age := now.Unix() - state.AppliedAt; age > 0 {
		m.AgeSeconds = age
	}
	if err := s.conn().QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM cluster_settings),
		        (SELECT COUNT(*) FROM secrets),
		        (SELECT COUNT(*) FROM users)`).
		Scan(&m.Settings, &m.Secrets, &m.Users); err != nil {
		return nil, fmt.Errorf("count mirrored rows: %w", err)
	}
	return m, nil
}

// Human is the one-line label every surface prints, so the CLI and doctor describe the
// same state with the same words: "from nxt-sw-5-m, version 42, applied 12s ago".
func (m *MirrorReading) Human() string {
	if m == nil {
		return "no snapshot applied on this node"
	}
	// "applied just now" rather than "just now ago": this line is read by an operator
	// during an incident, and a doubled adverb reads as a bug in the tool.
	if m.AgeSeconds < 1 {
		return fmt.Sprintf("from %s, version %d, applied just now", m.Leader, m.Version)
	}
	return fmt.Sprintf("from %s, version %d, applied %s ago", m.Leader, m.Version, durationHuman(m.AgeSeconds))
}

// durationHuman renders a positive number of seconds compactly. It never prints a
// fraction of a second and never prints "1.2e+06s": an operator reads this line.
func durationHuman(sec int64) string {
	switch {
	case sec < 90:
		return fmt.Sprintf("%ds", sec)
	case sec < 60*60:
		return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
	default:
		return fmt.Sprintf("%dh%02dm", sec/3600, (sec%3600)/60)
	}
}
