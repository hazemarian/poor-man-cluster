package api

import (
	"net"
	"net/http"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// isLoopback is the whole access control for the decision endpoint: the caller
// must be the root agent on this machine, and that is the only thing on this
// machine that asks. A nil IP (a bare "127.0.0.1" with no port) is refused
// rather than assumed local.
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// BackupDecisionHandler answers "should this node back up right now?" for the
// node-local agent.
//
// Unauthenticated on purpose, and restricted to loopback: the caller is a root
// process on the same machine, and a token would have to live in a file the
// agent already trusts. The answer carries no credentials — only switches and
// the schedule string — so the worst a local user learns is when backups run.
func BackupDecisionHandler(st *store.Store, now func() int64) http.HandlerFunc {
	// nil means wall clock; tests inject a fixed instant.
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r.RemoteAddr) {
			http.Error(w, "local only", http.StatusForbidden)
			return
		}
		t := time.Now()
		if now != nil {
			t = time.Unix(now(), 0)
		}
		writeJSON(w, http.StatusOK, cluster.BackupDecisionFor(r.Context(), st, t))
	}
}
