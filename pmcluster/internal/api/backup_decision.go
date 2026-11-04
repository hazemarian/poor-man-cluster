package api

import (
	"net"
	"net/http"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// isClusterReachable is the whole access control for the decision endpoint.
//
// Not loopback-only, deliberately: a worker's daemon stands by and serves no
// HTTP at all, so the agent on a worker has to ask a MANAGER — over the cluster
// plane, from a private address. Loopback covers the manager's own node. The
// public edge never proxies /internal/, and the answer carries switches and a
// schedule string, never a credential.
func isClusterReachable(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
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
		if !isClusterReachable(r.RemoteAddr) {
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
