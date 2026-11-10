package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runners"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/socketproxy"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// The per-node runner machinery.
//
// It runs on EVERY node and it starts BEFORE the leadership wait, for the same
// reason publishBackupPlan and the version publisher do (serve.go): a fence
// manager blocks in waitForSwarmLeadership forever, and a WORKER blocks there by
// design — it is never a candidate for the control loop. Everything placed after
// that wait therefore only ever ran on the leader, which is why the runner feature
// shipped in v0.2.267 was inert everywhere: measured on nxt-sw-6-w and nxt-sw-3-w,
// nothing listened on 127.0.0.1:9090 and /run/pmcluster/ did not exist.
//
// Two modes, one entry point:
//
//   - a WORKER gets its decision from the leader (it cannot read cluster settings
//     — configs are manager-only), so it runs runners.Agent against the
//     assignment it fetches over the swarm's mTLS channel;
//   - a manager/standalone node keeps the store-based reconcile it always had,
//     which now starts as soon as the store is open instead of never (on a
//     standby) or only after leadership (on the leader).

// lateStore is a handle that is resolved after the leadership wait. The daemon
// deliberately does not open the store before then (a standby's DB would be
// stale, and on promotion the restored control plane must be what gets opened),
// so the per-node loops take a getter instead of a *store.Store.
type lateStore struct {
	mu sync.RWMutex
	st *store.Store
}

func (l *lateStore) set(st *store.Store) {
	l.mu.Lock()
	l.st = st
	l.mu.Unlock()
}

func (l *lateStore) get() *store.Store {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.st
}

// socketPathSource answers "where does the filtered docker socket live on this
// node?" from the only source this node has: the store on a manager, the leader's
// assignment on a worker. Falls back to the platform default, which is what the
// daemon binds before anything has been read.
type socketPathSource struct {
	Store func() *store.Store

	mu         sync.RWMutex
	assignment string
}

func (s *socketPathSource) setAssignment(path string) {
	s.mu.Lock()
	s.assignment = path
	s.mu.Unlock()
}

func (s *socketPathSource) Path(ctx context.Context) string {
	if s.Store != nil {
		if st := s.Store(); st != nil {
			return cluster.LoadGitHubRunnerSocketPath(ctx, st)
		}
	}
	s.mu.RLock()
	p := s.assignment
	s.mu.RUnlock()
	if strings.TrimSpace(p) != "" {
		return p
	}
	return cluster.DefaultGitHubRunnerSocketPath
}

// assignmentFetcher is this node's half of the assignment channel. The production
// implementation is *runners.Client; a test substitutes one that answers without a
// leader, which is the only way to assert "the loop ran before leadership was
// resolved" without a sleep.
type assignmentFetcher interface {
	Fetch(ctx context.Context) (runners.Assignment, string, error)
}

// perNodeDeps is what the per-node loops need. Every dependency is optional so a
// test can wire one loop without the other.
type perNodeDeps struct {
	// Worker is true when this node is a swarm WORKER: it has no store and learns
	// its runner decision from the leader.
	Worker bool
	// Hostname is this node's name (the runner name's stable half).
	Hostname string
	// Container is the local container runtime the agent drives.
	Container runners.LocalDocker
	// Assignment fetches this node's assignment (nil on a manager).
	Assignment assignmentFetcher
	// StatePath is where the worker records the applied assignment (0600).
	StatePath string
	// SocketPath answers the filtered-socket path (shared with the supervisor).
	SocketPath *socketPathSource
	// Interval overrides the first poll interval (tests); the leader's answer
	// replaces it after the first fetch.
	Interval time.Duration
	// UpstreamPath overrides the real docker socket the proxy fronts (tests).
	UpstreamPath string

	Log zerolog.Logger
}

// perNodeLoops is the handle the caller (and the tests) can watch.
type perNodeLoops struct {
	// Proxy is the filtered docker socket listener.
	Proxy *socketproxy.Proxy
	// FirstAssignment is closed after the worker's first assignment pass, so a
	// test can assert on what it did without sleeping.
	FirstAssignment chan struct{}
	// assignmentFetches counts successful assignment fetches.
	assignmentFetches atomic.Int64
}

// Fetches reports how many assignments were successfully fetched.
func (l *perNodeLoops) Fetches() int64 { return l.assignmentFetches.Load() }

// startPerNodeLoops starts the socket proxy and the runner machinery. The proxy's
// first bind happens SYNCHRONOUSLY (it is local and fast, and a test asserts the
// socket exists before leadership is resolved); the runner half runs in its own
// goroutine and reports its first pass on FirstAssignment.
func startPerNodeLoops(ctx context.Context, d perNodeDeps) *perNodeLoops {
	lp := &perNodeLoops{
		Proxy: socketproxy.New(socketproxy.Config{
			SocketPath:   socketPathOrDefault(d.SocketPath, ctx),
			UpstreamPath: d.UpstreamPath,
			Log:          d.Log.With().Str("component", "socketproxy").Logger(),
		}),
		FirstAssignment: make(chan struct{}),
	}

	// The filtered socket is present on EVERY node with a docker socket,
	// unconditionally: it is inert when nothing mounts it, costs one goroutine,
	// and having it everywhere means a runner enabled on a node can never find
	// no socket because that daemon predates the setting.
	//
	// The first bind is synchronous so the socket exists before this function
	// returns — the runner loop reads it immediately, and a test can assert it
	// without sleeping.
	lp.bindProxy(ctx, d)
	go lp.superviseProxy(ctx, d)

	if d.Worker {
		if d.Assignment == nil {
			close(lp.FirstAssignment)
		} else {
			go lp.runAssignmentAgent(ctx, d)
		}
	}
	return lp
}

// bindProxy binds the filtered socket when this node has a docker socket to
// front. A node without one is not an error: binding a listener that cannot
// forward would turn every runner build into a 502 with no explanation.
func (lp *perNodeLoops) bindProxy(ctx context.Context, d perNodeDeps) {
	if _, err := os.Stat(lp.Proxy.UpstreamPath); err != nil {
		lp.Proxy.Log.Debug().Str("upstream", lp.Proxy.UpstreamPath).
			Msg("socket proxy: no docker socket on this node; not listening")
		return
	}
	if err := lp.Proxy.Start(ctx); err != nil {
		// A listener that cannot bind is a WARN, never a fatal: the daemon is
		// still the API, and the assignment keeps this node from registering a
		// runner it could not serve.
		lp.Proxy.Log.Warn().Err(err).Msg("socket proxy: not listening; runners on this node will not be registered")
	}
}

// superviseProxy keeps the listener on the path this node currently wants. The
// path is a setting on a manager and the leader's assignment on a worker, so it
// can change without a daemon restart.
func (lp *perNodeLoops) superviseProxy(ctx context.Context, d perNodeDeps) {
	want := ""
	if d.SocketPath != nil {
		want = d.SocketPath.Path(ctx)
	}
	if want != "" {
		lp.rebind(ctx, d, want)
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := lp.Proxy.Stop(); err != nil {
				lp.Proxy.Log.Warn().Err(err).Msg("socket proxy: could not stop cleanly")
			}
			return
		case <-t.C:
			if d.SocketPath == nil {
				continue
			}
			if p := d.SocketPath.Path(ctx); p != want {
				lp.rebind(ctx, d, p)
				want = p
			}
		}
	}
}

// rebind moves the listener to path (the socket proxy path changed).
func (lp *perNodeLoops) rebind(ctx context.Context, d perNodeDeps, path string) {
	if !lp.Proxy.Running() {
		if _, err := os.Stat(lp.Proxy.UpstreamPath); err == nil {
			lp.Proxy.SocketPath = path
			if err := lp.Proxy.Start(ctx); err != nil {
				lp.Proxy.Log.Warn().Err(err).Str("socket", path).Msg("socket proxy: not listening; runners on this node will not be registered")
			}
		}
		return
	}
	if lp.Proxy.SocketPath == path {
		return
	}
	if err := lp.Proxy.Stop(); err != nil {
		lp.Proxy.Log.Warn().Err(err).Msg("socket proxy: could not stop cleanly")
	}
	lp.Proxy.SocketPath = path
	if err := lp.Proxy.Start(ctx); err != nil {
		lp.Proxy.Log.Warn().Err(err).Str("socket", path).Msg("socket proxy: not listening; runners on this node will not be registered")
	}
}

func socketPathOrDefault(src *socketPathSource, ctx context.Context) string {
	if src == nil {
		return cluster.DefaultGitHubRunnerSocketPath
	}
	return src.Path(ctx)
}

// runAssignmentAgent is the worker's half: poll the leader, apply the answer.
//
// A failure keeps the LAST applied state and logs one bounded WRN — a leader
// mid-failover must not tear a running runner down, and must not fill the journal
// either.
func (lp *perNodeLoops) runAssignmentAgent(ctx context.Context, d perNodeDeps) {
	log := d.Log.With().Str("component", "runners").Logger()
	agent := &runners.Agent{
		Node:      d.Hostname,
		Docker:    d.Container,
		StatePath: d.StatePath,
		Log:       log,
	}
	interval := d.Interval
	if interval <= 0 {
		interval = runners.DefaultAssignmentIntervalSeconds * time.Second
	}

	var firstPass sync.Once
	markFirst := func() { firstPass.Do(func() { close(lp.FirstAssignment) }) }

	warn := &boundedWarn{logger: log, every: 5 * time.Minute}
	intervals := &intervalHolder{}
	intervals.set(interval)

	var sourceLogged bool
	tick := func() {
		// FirstAssignment means "the first pass finished", whatever it found — a
		// watcher waiting on it must not hang because the leader was unreachable.
		defer markFirst()
		a, source, err := d.Assignment.Fetch(ctx)
		if err != nil {
			if errors.Is(err, runners.ErrNoSwarmCertificate) {
				// Not a swarm node (or Docker has not provisioned its
				// certificate): one WRN per boot and no polling. There is no
				// unauthenticated fallback — an anonymous "what does this
				// cluster run" endpoint is exactly what this design refuses.
				log.Warn().Err(err).
					Msg("runners — this host holds no swarm node certificate, so it cannot read a runner assignment; not polling (nothing is started on this node)")
				intervals.set(0) // 0 = stop
				return
			}
			warn.log("runners — could not read this node's runner assignment; keeping the last state", err)
			return
		}
		if !sourceLogged {
			log.Info().Str("source", source).Str("node", d.Hostname).
				Msg("runners — runner assignment read from the leader over the swarm's mutual TLS channel")
			sourceLogged = true
		}
		if d.SocketPath != nil {
			d.SocketPath.setAssignment(a.SocketPath)
		}
		lp.assignmentFetches.Add(1)
		agent.RecordFetch(source, time.Now())
		res, applyErr := agent.Apply(ctx, a)
		if applyErr != nil {
			warn.log("runners — could not apply this node's runner assignment", applyErr)
		}
		if len(res.Registered) > 0 || len(res.Removed) > 0 {
			log.Info().Strs("registered", res.Registered).Strs("removed", res.Removed).
				Bool("enabled", a.Enabled).Str("reason", a.Reason).
				Msg("runners — this node's runner reconciled from the assignment")
		}
		intervals.set(a.Interval())
	}

	go func() {
		tick()
		for {
			wait := intervals.get()
			if wait <= 0 {
				markFirst()
				return
			}
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				markFirst()
				return
			case <-t.C:
			}
			tick()
		}
	}()
}

// intervalHolder is the poll interval as the last assignment decided it. Zero
// means "stop polling".
type intervalHolder struct {
	mu sync.RWMutex
	d  time.Duration
}

func (h *intervalHolder) set(d time.Duration) {
	h.mu.Lock()
	h.d = d
	h.mu.Unlock()
}

func (h *intervalHolder) get() time.Duration {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.d
}

// boundedWarn logs the first failure and then at most one line per window, with
// the number it suppressed. A leader that is down for an hour produces a dozen
// lines, not a journal full of them.
type boundedWarn struct {
	logger zerolog.Logger
	every  time.Duration
	mu     sync.Mutex
	last   time.Time
	seen   int
}

func (b *boundedWarn) log(msg string, err error) {
	b.mu.Lock()
	now := time.Now()
	if !b.last.IsZero() && now.Sub(b.last) < b.every {
		b.seen++
		b.mu.Unlock()
		return
	}
	seen := b.seen
	b.seen = 0
	b.last = now
	b.mu.Unlock()
	ev := b.logger.Warn().Err(err)
	if seen > 0 {
		ev = ev.Int("suppressed", seen)
	}
	ev.Msg(msg)
}

// isStandby reports whether this node must serve its API on loopback only: a
// swarm WORKER always, and a manager that is not (or cannot prove it is) the
// leader. Fail-safe in the direction that matters — a node that cannot answer "am
// I the leader?" must not bind all interfaces and answer cluster questions from a
// stale store.
func isStandby(worker, leader, found bool, err error) bool {
	if worker {
		return true
	}
	return err == nil && found && !leader
}

// apiAddr is the address this node's daemon API binds.
//
// The leader binds what the operator configured (0.0.0.0:9090 by default: the
// edge container reaches it through host.docker.internal, and Traefik routes
// pmcluster.<domain> to it). A WORKER binds the same port on loopback only: it is
// standing by, so its API answers nothing cluster-wide, but the runner loop needs
// a local surface and doctor/`/health` must work on every node. Exposing that off
// the host would turn every worker into a second, stale API — never.
func apiAddr(configured string, worker bool) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(configured))
	if err != nil {
		// A bare port ("9090") or a value we cannot parse: keep the port if we
		// can see one, otherwise fall back to the platform default.
		port = "9090"
		if p := strings.TrimPrefix(strings.TrimSpace(configured), ":"); p != "" && isDigits(p) {
			port = p
		}
		host = ""
	}
	if !worker {
		if host == "" {
			return net.JoinHostPort("0.0.0.0", port)
		}
		return net.JoinHostPort(host, port)
	}
	return net.JoinHostPort("127.0.0.1", port)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// assignmentAddr is where the leader's assignment listener binds: the node's
// swarm data-path address, never a public host port. A node without an overlay
// address (standalone) binds loopback, which is all it needs.
func assignmentAddr(nodeAddr string) string {
	nodeAddr = strings.TrimSpace(nodeAddr)
	if nodeAddr == "" {
		return net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", runners.AssignmentPort))
	}
	if h, _, err := net.SplitHostPort(nodeAddr); err == nil {
		nodeAddr = h
	}
	return net.JoinHostPort(nodeAddr, fmt.Sprintf("%d", runners.AssignmentPort))
}

// managerHosts returns the swarm manager addresses a worker can try, from the
// live swarm itself (`docker info`): the addresses Swarm already uses between
// nodes, so no DNS, no published port and no firewall change is involved.
func managerHosts(ctx context.Context, dc runtime.Client) ([]string, error) {
	if dc == nil {
		return nil, errors.New("no docker client: cannot read the manager addresses")
	}
	info, err := dc.Info(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, addr := range info.SwarmRemoteManagers {
		if h, _, err := net.SplitHostPort(addr); err == nil {
			out = append(out, h)
		} else if addr != "" {
			out = append(out, addr)
		}
	}
	if len(out) == 0 && info.SwarmNodeAddr != "" {
		// A single-manager swarm reports no remote managers; its own data-path
		// address is the only candidate (and on a worker it is not ours, so this
		// is a best effort, not a guarantee).
		out = append(out, info.SwarmNodeAddr)
	}
	return out, nil
}

// agentStatePath is where a worker records the assignment it applied. The dir is
// the daemon's data dir (0700), the file is 0600: it names the node, the org and
// the socket path, which is nobody else's business on a shared host.
func agentStatePath(dataDir string) string {
	if strings.TrimSpace(dataDir) == "" {
		return ""
	}
	return dataDir + string(os.PathSeparator) + "runner_assignment.json"
}
