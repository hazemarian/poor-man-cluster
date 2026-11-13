package clustersnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// The follower's half of the channel: one conditional GET per poll, decoded into the
// same projection the handler writes. It lives beside the handler on purpose — the
// wire shape (Body), the ETag composition and the status meanings are one contract,
// and a client that re-declares them is the copy that drifts when the snapshot grows
// a table.

// Doer performs one request against the leader's node-facing listener. The
// implementation walks the candidate manager addresses (the runner assignment's
// transport, in internal/runners) and returns the FIRST HTTP response it gets,
// status included: a 403 from the leader is an answer about this node, not a
// transport failure, so only a connect/TLS error may move on to the next candidate.
type Doer interface {
	Do(ctx context.Context, method, path string, header map[string]string) (status int, respHeader http.Header, body []byte, base string, err error)
}

// PollInterval is the cadence of the follower's pull, and it lives here because two
// things depend on it and they must agree: the loop that polls (internal/cli) and the
// staleness threshold a read surface applies when it labels a mirrored answer (three
// intervals, in internal/doctor). It matches the runner-assignment interval — one number
// for every node-facing pull — and it is a constant rather than a setting because the
// follower cannot read settings, which is the whole reason it mirrors.
const PollInterval = 60 * time.Second

// Client reads the leader's snapshot. One poll, one request, close.
type Client struct {
	Do Doer
	// Log is used for the one non-fatal anomaly worth recording on this side: a
	// leader whose answer cannot be decoded is a version skew during a rolling
	// upgrade, and silence there reads as "the mirror is current".
	Log zerolog.Logger
}

// Result is one poll's outcome.
type Result struct {
	// Snapshot is the decoded projection; nil when NotModified.
	Snapshot *store.Snapshot
	// Leader is the hostname the leader named for itself. Recorded with the rows
	// because a version number is only meaningful relative to its store.
	Leader string
	// ETag is the tag this version answers with, ready to send as If-None-Match on
	// the next poll.
	ETag string
	// NotModified reports a 304: the follower already holds this version, so there is
	// nothing to apply and the poll cost a few dozen bytes.
	NotModified bool
}

// Fetch asks the leader for the current snapshot, conditional on ifNoneMatch (pass ""
// to force a full read).
func (c *Client) Fetch(ctx context.Context, ifNoneMatch string) (Result, error) {
	if c.Do == nil {
		return Result{}, errors.New("snapshot client: no transport")
	}
	header := map[string]string{"Accept": "application/json"}
	if ifNoneMatch = strings.TrimSpace(ifNoneMatch); ifNoneMatch != "" {
		header["If-None-Match"] = ifNoneMatch
	}
	status, respHeader, body, base, err := c.Do.Do(ctx, http.MethodGet, Path, header)
	if err != nil {
		return Result{}, fmt.Errorf("read the cluster snapshot: %w", err)
	}
	switch status {
	case http.StatusNotModified:
		return Result{NotModified: true, ETag: respHeader.Get("ETag")}, nil
	case http.StatusOK:
	case http.StatusForbidden:
		return Result{}, fmt.Errorf("%s refused this node's certificate: %s", base, apiErrorText(body))
	default:
		return Result{}, fmt.Errorf("%s answered %d: %s", base, status, apiErrorText(body))
	}

	var wire Body
	if err := json.Unmarshal(body, &wire); err != nil {
		return Result{}, fmt.Errorf("decode the snapshot from %s: %w", base, err)
	}
	if strings.TrimSpace(wire.Leader) == "" {
		return Result{}, fmt.Errorf("the snapshot from %s names no leader", base)
	}
	generatedAt, err := time.Parse(time.RFC3339, wire.GeneratedAt)
	if err != nil {
		return Result{}, fmt.Errorf("the snapshot from %s carries generated_at %q: %w", base, wire.GeneratedAt, err)
	}
	return Result{
		Snapshot: &store.Snapshot{
			Version:     wire.Version,
			GeneratedAt: generatedAt.Unix(),
			Settings:    wire.Tables.Settings,
			Secrets:     wire.Tables.Secrets,
			Users:       wire.Tables.Users,
		},
		Leader: wire.Leader,
		// Derived from the body's own two values, exactly as the server derived the
		// header: the next poll sends this back and must match.
		ETag: ETag(wire.Leader, wire.Version),
	}, nil
}

// apiErrorText pulls the short reason out of an error body, bounded so a proxy page
// cannot land in a log line.
func apiErrorText(body []byte) string {
	var e struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &e); err == nil {
		msg := strings.TrimSpace(e.Error)
		if r := strings.TrimSpace(e.Reason); r != "" {
			if msg != "" {
				msg += " (" + r + ")"
			} else {
				msg = r
			}
		}
		if msg != "" {
			return msg
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
