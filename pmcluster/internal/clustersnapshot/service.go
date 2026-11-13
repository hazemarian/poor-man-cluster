// Package clustersnapshot serves the leader's cluster state to the managers that
// mirror it, over the swarm's existing mutual-TLS channel.
//
// It is the read half of one decision recorded in
// pmcluster/docs/adr/0003-cluster-state-by-pull.md: cluster settings (and the
// cipher key that decrypts secrets) are copied to every node today by writing
// them into Docker configs, which means the key sits in a Raft-replicated object
// any manager can read and every settings write is a config rotation against a
// 512 KB ceiling. The replacement is one writer (the leader's store) and readers
// that PULL.
//
// Three properties this package is responsible for:
//
//  1. It answers only on the swarm-mTLS listener, behind swarmtls.Verifier, and
//     never on the public /api router. The caller's identity comes from its node
//     CERTIFICATE — there is no query parameter and no body field that can name a
//     node, ask for another node's view, or widen the answer.
//  2. It answers a PROJECTION, not the database: three tables a mirror carries,
//     and nothing that describes another machine's containers, an archive it
//     cannot see, or a log. The store type says what and why.
//  3. It is cheap when nothing changed. `version` is both the body's version and
//     the ETag, so an `If-None-Match` naming the current one gets a 304 with no
//     body — the point of a 60-second poll.
package clustersnapshot

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth/swarmtls"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// Path is the route, under the internal namespace: served by the daemon on the
// swarm-mTLS listener (never on the public API) and only while this node is the
// leader — the listener itself is started after the leadership wait.
const Path = "/internal/api/cluster/snapshot"

// Service answers one snapshot request.
type Service struct {
	// Store is this leader's control-plane store. It is the only writer of the
	// version and of the rows the body carries.
	Store *store.Store
	// Hostname is this node's hostname: sent as `leader` so a follower can record
	// WHICH leader's state it mirrored. A mirror that only recorded a version
	// number could not tell a leadership change from a stall.
	Hostname string
	Log      zerolog.Logger
}

// Body is the wire shape, shared by this handler and the follower's client so the
// two cannot drift: a struct rather than a map, so a field cannot be added to the
// snapshot without appearing here, and the ETag is derived from the same two values
// the body carries, so a header and a body can never disagree.
type Body struct {
	Version     int64  `json:"version"`
	GeneratedAt string `json:"generated_at"`
	Leader      string `json:"leader"`
	Tables      Tables `json:"tables"`
}

// Tables keeps every table ALWAYS present, empty lists included. A follower has to
// be able to read "this table is empty" and delete every row it mirrors; an absent
// or null key would be indistinguishable from "the leader did not send that table",
// and the difference between those two is every deleted row.
type Tables struct {
	Settings []store.SnapshotSetting `json:"settings"`
	Secrets  []store.SnapshotSecret  `json:"secrets"`
	Users    []store.SnapshotUser    `json:"users"`
}

// ServeHTTP answers GET with the current snapshot, or 304 when the caller already
// has that version.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "the snapshot is GET only"})
		return
	}
	ident, ok := swarmtls.FromContext(r.Context())
	if !ok {
		// The route is mounted behind swarmtls.Verifier; reaching here without an
		// identity means it was mounted somewhere else, which is a wiring bug worth
		// failing loudly rather than answering.
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "no authenticated swarm node"})
		return
	}
	// A manager, not any node. The body carries the cluster's settings, and among
	// them a few secret values (the offsite keys, the SSO client secret) that the
	// console masks — the same material a manager already holds in the Raft
	// control-plane kit. A WORKER has no store to mirror into and nothing in this
	// body it needs: it is told what to do through the runner assignment. Refusing
	// is a narrowing, not an oversight, and saying so means an operator with a
	// mis-set role gets a reason instead of a mystery.
	if !strings.EqualFold(strings.TrimSpace(ident.Role), "manager") {
		s.Log.Warn().Str("node", ident.Hostname).Str("role", ident.Role).
			Msg("cluster snapshot refused: the caller is not a manager, and only a manager has a store to mirror into")
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":  "forbidden",
			"reason": "the cluster snapshot is served to managers only (a worker has no store to mirror it into)",
		})
		return
	}
	if s.Store == nil {
		// Not something the caller can fix: accept nothing, answer nothing, and let
		// the log carry it. A leader without a store is a broken daemon.
		s.Log.Error().Msg("cluster snapshot unavailable: this daemon has no store")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "this leader has no store to read the snapshot from"})
		return
	}

	snap, err := s.Store.Snapshot(r.Context())
	if err != nil {
		s.Log.Error().Err(err).Msg("cluster snapshot: read failed")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "snapshot read failed"})
		return
	}

	// The ETag is (leader, version) and not the version alone. A version number is
	// only meaningful relative to the store that produced it: after a leadership
	// change the new leader's counter may equal the old one while the content
	// differs, and a version-only tag would answer 304 to a follower that holds a
	// different leader's data. A client must therefore remember the PAIR it applied,
	// which is why the body carries `leader` beside `version`.
	etag := ETag(s.Hostname, snap.Version)
	w.Header().Set("ETag", etag)
	if matchesETag(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	writeJSON(w, http.StatusOK, Body{
		Version:     snap.Version,
		GeneratedAt: time.Unix(snap.GeneratedAt, 0).UTC().Format(time.RFC3339),
		Leader:      s.Hostname,
		Tables: Tables{
			Settings: snap.Settings,
			Secrets:  snap.Secrets,
			Users:    snap.Users,
		},
	})
}

// ETag is the one place the tag is composed, so the header and any client helper
// derive it identically: a client that composed it differently would never match a
// 304 and would re-download the cluster state on every poll.
func ETag(leader string, version int64) string {
	return `"` + leader + ":" + strconv.FormatInt(version, 10) + `"`
}

// matchesETag reports whether an If-None-Match header names want.
//
// The comparison is the weak one RFC 9110 defines for If-None-Match: the W/ prefix
// is part of neither the value nor the identity, a list is allowed, and `*` matches
// any existing representation. Doing this by string equality alone is the bug that
// makes a cache silently never hit.
func matchesETag(header, want string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	want = strings.TrimSpace(strings.TrimPrefix(want, "W/"))
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		if strings.TrimPrefix(candidate, "W/") == want {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
