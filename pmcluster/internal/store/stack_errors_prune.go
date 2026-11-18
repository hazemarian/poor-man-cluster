package store

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// levelListPattern matches the one message shape whose text varies without the failure changing: the
// depends_on level list is built from a map iteration, so the same timeout reads "[db, web]" on one
// pass and "[web, db]" on the next. That variation defeated the collapse in RecordStackError and left
// histories with one entry per reconcile pass for a single broken deploy.
var levelListPattern = regexp.MustCompile(`waiting for depends_on level \[([^\]]*)\] to become ready`)

// healthyListPattern is the CURRENT wording ("waiting for [web] to become healthy"). Both shapes are
// normalised: histories carry the old one for months after a rename, and a comparison that only knows
// the new spelling would let the same failure accumulate again as two different errors.
var healthyListPattern = regexp.MustCompile(`waiting for \[([^\]]*)\] to become healthy`)

// canonicalStackError rewrites a failure message into the form that is comparable across attempts.
// It changes ONLY the parts that describe how a pass happened, never what failed.
func canonicalStackError(msg string) string {
	// The diagnosis a deploy appends ("— web: 0/1 running, newest task failed (…)") names the
	// evidence at one moment: a task error that repeats with a new pid, a replica count that moves.
	// Two entries describing the same cause must compare equal, so the suffix is dropped for the
	// KEY — the stored message keeps it.
	for _, marker := range []string{" to become ready", " to become healthy"} {
		if at := strings.Index(msg, marker); at >= 0 {
			if cut := strings.Index(msg[at:], " — "); cut >= 0 {
				msg = msg[:at+cut]
			}
			break
		}
	}
	msg = healthyListPattern.ReplaceAllStringFunc(msg, func(m string) string {
		parts := strings.Split(healthyListPattern.FindStringSubmatch(m)[1], ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		sort.Strings(parts)
		return "waiting for [" + strings.Join(parts, ", ") + "] to become healthy"
	})
	return levelListPattern.ReplaceAllStringFunc(msg, func(m string) string {
		parts := strings.Split(levelListPattern.FindStringSubmatch(m)[1], ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		sort.Strings(parts)
		return "waiting for depends_on level [" + strings.Join(parts, ", ") + "] to become ready"
	})
}

// PruneDuplicateStackErrors merges entries in a stack's error history that describe the SAME failure
// on the SAME revision, summing their attempt counts and keeping the newest timestamp. It exists for
// histories written before messages were canonical and before RecordStackError collapsed retries: on
// this cluster one stack carried twenty entries for one timeout.
//
// Returns how many entries were removed and how many remain. Idempotent: a second run removes nothing.
func (s *Store) PruneDuplicateStackErrors(ctx context.Context, stackName string) (removed, kept int, err error) {
	var raw string
	if err := s.conn().QueryRowContext(ctx, `SELECT last_error FROM stacks WHERE name = ?`, stackName).Scan(&raw); err != nil {
		return 0, 0, fmt.Errorf("read stack last_error: %w", err)
	}
	entries := ParseStackErrors(raw)
	if len(entries) == 0 {
		return 0, 0, nil
	}

	out := make([]StackErrorEntry, 0, len(entries))
	index := make(map[string]int, len(entries))
	for _, e := range entries {
		count := e.Count
		if count == 0 {
			count = 1
		}
		key := fmt.Sprintf("%d\x00%s", e.Revision, canonicalStackError(e.Error))
		if at, seen := index[key]; seen {
			out[at].Count += count
			if e.CreatedAt > out[at].CreatedAt {
				out[at].CreatedAt = e.CreatedAt
			}
			removed++
			continue
		}
		e.Count = count
		// The message is kept VERBATIM: it is the newest attempt's, and the one that carries the
		// diagnosis. Canonicalisation is for comparison only.
		index[key] = len(out)
		out = append(out, e)
	}
	if removed == 0 {
		return 0, len(out), nil
	}
	if len(out) > StackErrorHistoryLimit {
		out = out[:StackErrorHistoryLimit]
	}
	b, merr := json.Marshal(out)
	if merr != nil {
		return 0, 0, fmt.Errorf("marshal pruned history: %w", merr)
	}
	if werr := s.SetStackLastError(ctx, stackName, string(b)); werr != nil {
		return 0, 0, werr
	}
	return removed, len(out), nil
}
