package cli

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth/swarmtls"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/clustersnapshot"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodereport"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runners"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/server"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// The two halves of the assignment channel that live in the daemon: the leader
// serves it (on a dedicated mTLS listener, on one route), a worker fetches it.

// assignmentClientFor builds the worker's fetcher. A manager gets nil: it reads
// the settings itself, and a manager fetching its own assignment would be a
// second code path for the same decision.
// assignmentClientFor builds the client every node that is NOT the leader uses to learn
// its runner decision. That used to mean "a worker" only, which left a following manager
// with no path at all: it has no current settings to read, so selecting it in the console
// produced nothing, forever, with no error anywhere. The leader keeps using its store.
func assignmentClientFor(dc runtime.Client) assignmentFetcher {
	if dc == nil {
		return nil
	}
	return &runners.Client{
		Managers: func(ctx context.Context) ([]string, error) { return managerHosts(ctx, dc) },
	}
}

// startWorkerAPI serves the daemon's API on the given (loopback) address while a
// worker stands by.
//
// It is a store-less daemon on purpose: a worker has no current control plane, and
// opening its data directory would seed an empty database whose answers look
// plausible — the exact failure the CLI's leader guard exists to prevent. So this
// listener answers /health and nothing else, and the runner loop gets the local
// surface it needs without pretending to be a leader.
func startWorkerAPI(ctx context.Context, dc runtime.Client, addr string, log zerolog.Logger) (func(), <-chan struct{}) {
	h := server.New(server.Deps{Docker: dc})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Run(runCtx, addr, h); err != nil && runCtx.Err() == nil {
			log.Warn().Err(err).Str("addr", addr).
				Msg("worker API: not serving on loopback; the runner loop and /health have no local surface")
		}
	}()
	log.Info().Str("addr", addr).Msg("worker API listening on loopback (standing by; cluster state lives on the leader)")
	return cancel, done
}

// assignmentListenerOptions is the leader's side wiring.
type assignmentListenerOptions struct {
	Store    *store.Store
	Hostname string
	Docker   runtime.Client
	GitHub   runners.GitHub
	// CacheReports collects what nodes report about their own cache volumes. Shared
	// with the runner manager so the console can show a runner on another machine.
	CacheReports *runners.CacheReports
	// Deregister removes a registration named after a node the leader stopped.
	Deregister func(ctx context.Context, org, name string) error
	// Held reports an operator stop recorded on this leader. The node cannot
	// read that roster, so the decision travels in the assignment.
	Held func(runnerName string) bool
	Log  zerolog.Logger

	// Test seams: override the bind address and the swarm CA/cert paths.
	Addr     string
	CAFile   string
	CertFile string
	KeyFile  string
}

// startAssignmentListener serves the node-facing routes — the runner assignment,
// the node report, and the cluster snapshot — to nodes of this swarm, over mutual
// TLS, on a dedicated listener bound to this node's swarm data-path address.
//
// Three properties this function is responsible for, each of which used to be a
// gap:
//
//  1. It is the ONLY listener swarmtls is mounted on, and every route on it is
//     behind the verifier. Not on /api/*, not on /metrics, not on the console:
//     a compromised worker gets three projections of what its own certificate
//     entitles it to, not a second API.
//  2. It binds the node's overlay address (from `docker info`), never a public
//     host port. The port is in harden-host.sh's restricted list so the WAN
//     cannot reach it even if a bind is ever widened.
//  3. It fails CLOSED and loudly: no swarm CA or no node certificate means the
//     endpoint is not served and one WARN says so — never an unauthenticated
//     fallback.
func startAssignmentListener(ctx context.Context, o assignmentListenerOptions) {
	log := o.Log.With().Str("component", "runner-assignment").Logger()
	if o.Docker == nil {
		log.Warn().Msg("runner assignment endpoint NOT served: no docker client on this daemon")
		return
	}
	caPaths := swarmtls.CAPaths
	if o.CAFile != "" {
		caPaths = []string{o.CAFile}
	}
	pool, caPath, err := swarmtls.LoadCA(caPaths...)
	if err != nil {
		log.Warn().Err(err).
			Msg("runner assignment endpoint NOT served: no swarm CA on this node (swarm nodes of this cluster cannot be authenticated)")
		return
	}
	certFile := o.CertFile
	if certFile == "" {
		certFile = swarmtls.FirstFile(swarmtls.NodeCertPaths)
	}
	keyFile := o.KeyFile
	if keyFile == "" {
		keyFile = swarmtls.FirstFile(swarmtls.NodeKeyPaths)
	}
	if certFile == "" || keyFile == "" {
		log.Warn().
			Msg("runner assignment endpoint NOT served: this node has no swarm node certificate to serve TLS with")
		return
	}
	addr := o.Addr
	if addr == "" {
		addr = assignmentAddr(selfSwarmAddr(ctx, o.Docker))
	}

	svc := &runners.AssignmentService{
		// The same sink the console reads: a node reports its cache sizes here, and
		// the leader shows them for a runner that lives on that node.
		CacheReports: o.CacheReports,
		Config: func(ctx context.Context) runners.Config {
			return runners.LoadConfig(ctx, o.Store, o.Hostname)
		},
		Nodes:      o.Docker,
		GitHub:     o.GitHub,
		Deregister: o.Deregister,
		Held:       o.Held,
		// A worker's RAM exists only in this leader's node-stats rows (the swarm
		// node list carries none), so the size check is resolved here, on the side
		// that decides. Unknown RAM allows the runner — see runners.CheckMemoryFit.
		NodeMemory: func(ctx context.Context, node string) (int64, bool) {
			return runners.NodeMemoryMiB(ctx, o.Store, node, time.Now())
		},
		Log: log,
	}
	verifier := &swarmtls.Verifier{
		CA:    pool,
		Nodes: o.Docker,
		SwarmID: func(ctx context.Context) (string, error) {
			return swarmIDFor(ctx, o.Docker, certFile)
		},
		Log: log,
	}
	// The node-facing routes, ALL of them behind this verifier and none of them
	// reachable anywhere else. Since the settings-snapshot work there are three, and
	// the rule they share is the one that matters: this listener is the ONLY place
	// swarmtls is mounted, so a compromised worker gains no second readable API, and
	// each route answers a projection of what THIS caller needs.
	mux := http.NewServeMux()
	mux.Handle(runners.AssignmentPath, verifier.Middleware(svc))
	// Node reports travel on the SAME listener and behind the SAME verifier: the console
	// is served by the leader, the numbers belong to the node that runs the workload, and
	// this is the only authenticated channel between them. /internal/api and not /api:
	// these routes are for nodes, and the public API's token auth must never be the thing
	// that stands between a node and its own measurement.
	mux.Handle(nodereport.Path, verifier.Middleware(&nodereport.Service{
		Store: o.Store,
		Log:   log.With().Str("component", "node-report").Logger(),
		// State only. The sample's telemetry half was already exported by the node that
		// took it, straight to the collector, so nothing here republishes it: this handler
		// writes the current value the panel reads and stops there.
	}))
	// The cluster snapshot: the read half of the pull-based replication that replaces
	// writing settings into docker configs (pmcluster/docs/adr/0003-cluster-state-by-pull.md).
	// The handler refuses anything but a MANAGER certificate — a worker has no store to
	// mirror into — and answers a version + ETag so an unchanged poll is a 304.
	mux.Handle(clustersnapshot.Path, verifier.Middleware(&clustersnapshot.Service{
		Store:    o.Store,
		Hostname: o.Hostname,
		Log:      log.With().Str("component", "cluster-snapshot").Logger(),
	}))

	serveCfg := swarmtls.ServeConfig{
		Addr:     addr,
		CertFile: certFile,
		KeyFile:  keyFile,
		CA:       pool,
		CAFile:   caPath,
		Handler:  mux,
		Log:      log,
	}
	go func() {
		if err := swarmtls.Serve(ctx, serveCfg); err != nil {
			log.Warn().Err(err).Str("addr", addr).Msg("runner assignment endpoint stopped")
		}
	}()
	log.Info().Str("addr", addr).Str("ca", caPath).
		Strs("paths", []string{runners.AssignmentPath, nodereport.Path, clustersnapshot.Path}).
		Msg("node endpoint listening (swarm mutual TLS, these routes only)")
}

// selfSwarmAddr is this node's swarm data-path address, or "" when it has none
// (standalone). Read per start, not cached: a node that rejoins gets a new one.
func selfSwarmAddr(ctx context.Context, dc runtime.Client) string {
	if dc == nil {
		return ""
	}
	info, err := dc.Info(ctx)
	if err != nil {
		return ""
	}
	return info.SwarmNodeAddr
}

// swarmIDFor is the cluster ID client certificates must carry in their
// organisation. A manager reads it from `docker info`; the fallback is its own
// certificate's organisation, which is the same value and works even when docker
// is busy.
func swarmIDFor(ctx context.Context, dc runtime.Client, certFile string) (string, error) {
	if dc != nil {
		if info, err := dc.Info(ctx); err == nil && strings.TrimSpace(info.SwarmClusterID) != "" {
			return info.SwarmClusterID, nil
		}
	}
	return certSwarmID(certFile)
}

// certSwarmID reads the swarm organisation from a certificate file. It mirrors
// the worker-side helper so both halves agree on where the cluster ID lives.
func certSwarmID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			return "", errNoSwarmOrg
		}
		if block.Type != "Certificate" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", err
		}
		if id := swarmtls.SwarmIDOf(cert); id != "" {
			return id, nil
		}
		return "", errNoSwarmOrg
	}
}

var errNoSwarmOrg = errors.New("certificate carries no swarm organisation")
