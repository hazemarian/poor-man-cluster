// Package nodereport accepts a node's own measurement over the swarm's mutual-TLS
// channel and keeps it where the console can read it.
//
// It exists because of a structural fact: the console is served by the leader, the
// numbers belong to the node that runs the workload, and a host process cannot reach an
// overlay-only service. The leader can measure exactly one machine — itself. So each node
// sends what only it can see, and the leader writes the row.
//
// The node's identity comes from the CERTIFICATE, never from the body. A node that sends
// a different hostname is corrected, not trusted: otherwise one machine can overwrite
// another's row and every panel lies.
package nodereport

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth/swarmtls"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodestat"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// Path is the route, under the internal namespace: it is served by the daemon on the
// swarm-mTLS listener, never on the public API, and it is never reachable from outside
// the cluster's node certificates.
const Path = "/internal/api/cluster/report"

// maxBody bounds one report. A sample is a few KB (two top-process lists); a megabyte is
// already generous and stops a corrupt or hostile node from exhausting the leader.
const maxBody = 1 << 20

// Service handles node reports.
type Service struct {
	Store *store.Store
	Log   zerolog.Logger
	// Now is the clock (tests).
	Now func() time.Time
}

type reportResponse struct {
	Node    string `json:"node"`
	Stored  bool   `json:"stored"`
	Ignored string `json:"ignored,omitempty"`
}

// ServeHTTP accepts one node sample.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// The channel is one-directional by design: read the assignment with GET,
		// push a measurement with POST. Anything else is a client bug, and saying so
		// beats a 404 that reads like a missing route.
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "report is POST only"})
		return
	}
	ident, ok := swarmtls.FromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "no authenticated swarm node"})
		return
	}
	node := strings.TrimSpace(ident.Hostname)
	if node == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "certificate is not a live swarm node"})
		return
	}
	if s.Store == nil {
		// Not an error the node can fix: say it plainly rather than accepting the
		// report and dropping it, which is how a panel goes quietly empty.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "this leader has no store to keep reports in"})
		return
	}

	var smp nodestat.Sample
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&smp); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode: " + err.Error()})
		return
	}

	// The certificate decides which row this is. A claim that disagrees is recorded as
	// ignored so a misconfigured node is visible instead of silently winning.
	ignored := ""
	if claimed := strings.TrimSpace(smp.Hostname); claimed != "" && !strings.EqualFold(claimed, node) {
		ignored = claimed
		s.Log.Warn().Str("cert", node).Str("claimed", claimed).
			Msg("node report claims a different hostname; the certificate wins")
	}
	smp.Hostname = node

	now := s.now()
	if smp.TS == 0 {
		smp.TS = now.Unix()
	}
	st := smp.Stat()
	if err := s.Store.UpsertNodeStats(r.Context(), []nodestat.Stat{st}, now.Unix()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, reportResponse{Node: node, Stored: true, Ignored: ignored})
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
