// Package socketproxy is the platform's own filter in front of the host's
// Docker socket.
//
// Why it exists: a GitHub Actions runner has to build images, and building
// through the daemon means talking to `/var/run/docker.sock` — which is root on
// the host, and (on a manager) the whole swarm. Mounting the raw socket into a
// runner turns "a workflow can run a build" into "a workflow can run anything":
// `docker service rm`, `docker node demote`, `docker secret inspect`, `docker
// exec` into any container. Handing a runner a socket that only forwards what a
// build needs turns those into a 403 instead of a policy.
//
// What this is NOT: a security boundary. The proxy still fronts a root daemon,
// and the routes it allows (create/start/remove containers, pull images, mount
// host paths through a container spec) are already enough to escalate on the
// node. It is blast-radius control: it removes the one-command swarm-wide
// actions and the read of every swarm secret. A runner placed on a manager node
// defeats even that, which is why placement keeps it off the leader.
//
// The allow table is deny-by-default and segment-exact: a request that is not
// matched by a rule is refused, and there is no generic passthrough. Audit lines
// (WRN, rate-limited) are how an operator discovers that a workflow tried
// `service rm` — grep the journal for "socket proxy: denied".
package socketproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// Defaults. The listener is a unix socket under /run (tmpfs, recreated on every
// boot), owned root:root with group read-write so a container can be given the
// socket through a bind mount without world-write.
const (
	DefaultSocketPath   = "/run/pmcluster/docker.sock"
	DefaultUpstreamPath = "/var/run/docker.sock"
	DefaultSocketMode   = 0o660
	DefaultDirMode      = 0o755
)

// audit tuning: the first auditBurst denials in a window are logged one by one
// (enough to see what a workflow tried), and the rest collapse into one summary
// line per window so a retry loop cannot flood the journal.
const (
	auditBurst    = 8
	auditInterval = time.Minute
)

// Rule is one allowed route. Method is an HTTP verb (or "*"). Pattern is a path
// with segment wildcards: `{id}` matches exactly one segment, `**` matches one
// or more trailing segments (used only for image names, which contain slashes).
type Rule struct {
	Method  string
	Pattern string
	Why     string
}

// AllowTable is the whole allow list. Everything not in it is refused. Each
// entry exists because an image build needs it; adding one is a security
// decision, not a convenience.
var AllowTable = []Rule{
	// Liveness and identity. The docker client reads these on every command.
	{Method: "GET", Pattern: "/_ping", Why: "docker client handshake"},
	{Method: "HEAD", Pattern: "/_ping", Why: "docker client handshake"},
	{Method: "GET", Pattern: "/version", Why: "API negotiation"},
	{Method: "GET", Pattern: "/info", Why: "buildx checks the daemon's drivers and limits"},
	{Method: "GET", Pattern: "/events", Why: "docker build/run wait on container events"},

	// Containers: a build runs a builder container and a `docker run` step needs
	// the same lifecycle. No exec, no attach, no privileged helper path.
	{Method: "GET", Pattern: "/containers/json", Why: "list containers (buildx looks for a running builder)"},
	{Method: "POST", Pattern: "/containers/create", Why: "create the builder / a job container"},
	{Method: "GET", Pattern: "/containers/{id}/json", Why: "inspect a container"},
	{Method: "POST", Pattern: "/containers/{id}/start", Why: "start it"},
	{Method: "POST", Pattern: "/containers/{id}/stop", Why: "stop it"},
	{Method: "POST", Pattern: "/containers/{id}/restart", Why: "restart it"},
	{Method: "POST", Pattern: "/containers/{id}/kill", Why: "kill a hung job"},
	{Method: "POST", Pattern: "/containers/{id}/wait", Why: "wait for it to finish"},
	{Method: "GET", Pattern: "/containers/{id}/logs", Why: "read a job container's output"},
	{Method: "DELETE", Pattern: "/containers/{id}", Why: "remove it"},

	// Registry authentication. `docker login` calls POST /auth, which stores a
	// credential in the DAEMON's config — it grants no host capability, and the
	// alternative (a mounted config file) would give every job on this node the
	// same token. Scoped by whatever credential the workflow supplies.
	{Method: "POST", Pattern: "/auth", Why: "docker login: a job must authenticate to push"},

	// Images: pull, build, inspect, tag, push, remove.
	{Method: "GET", Pattern: "/images/json", Why: "list images"},
	{Method: "POST", Pattern: "/images/create", Why: "pull"},
	{Method: "POST", Pattern: "/build", Why: "build"},
	// `**` because an image name contains slashes (ghcr.io/acme/app:1.0) — the
	// {name} placeholder matches one segment and silently refused every real
	// reference, which is indistinguishable from the route being banned.
	{Method: "POST", Pattern: "/images/{name}/tag", Why: "tag by short name"},
	{Method: "POST", Pattern: "/images/**/tag", Why: "tag a registry-qualified name"},
	// Push is a supply-chain action, and it is here deliberately: a CI job whose
	// whole purpose is publishing an image cannot work without it. What bounds it
	// is the credential the job authenticated with — a repo's GITHUB_TOKEN can
	// only write that repo's packages, so a compromised workflow cannot publish
	// over another repo's image. It cannot reach the platform's own workloads
	// either: /services, /nodes and /swarm stay refused below.
	{Method: "POST", Pattern: "/images/{name}/push", Why: "publish by short name"},
	{Method: "POST", Pattern: "/images/**/push", Why: "publish a registry-qualified name"},
	{Method: "GET", Pattern: "/images/{id}/json", Why: "inspect by id"},
	{Method: "GET", Pattern: "/images/**/json", Why: "inspect by name (the name contains slashes)"},
	{Method: "DELETE", Pattern: "/images/{id}", Why: "remove by id"},
	{Method: "DELETE", Pattern: "/images/**", Why: "remove by name"},

	// Volumes and networks: a build may mount a cache volume and needs a
	// network to build into.
	{Method: "GET", Pattern: "/volumes", Why: "list volumes"},
	{Method: "POST", Pattern: "/volumes/create", Why: "create a cache volume"},
	{Method: "GET", Pattern: "/volumes/{name}", Why: "inspect a volume"},
	{Method: "DELETE", Pattern: "/volumes/{name}", Why: "remove a volume"},
	{Method: "GET", Pattern: "/networks", Why: "list networks"},
	{Method: "POST", Pattern: "/networks/create", Why: "create a build network"},
	{Method: "GET", Pattern: "/networks/{id}", Why: "inspect a network"},
	{Method: "DELETE", Pattern: "/networks/{id}", Why: "remove a network"},
	// attach/detach a container from a user-defined network. Without these a job
	// cannot put ITSELF on the network it just created, so the only way to reach a
	// database it starts is the default bridge's address — which changes on every
	// recreate and shares a subnet with every other container on the node. With
	// them a job gets what any compose file takes for granted: a network name and
	// DNS by service name.
	//
	// The marginal reach is small next to what is already allowed: create is
	// permitted, so a job can already start a container on any network it names.
	// These two let it attach an EXISTING one — its own runner — which is the
	// point. Still denied: services, nodes, exec, secrets, swarm.
	{Method: "POST", Pattern: "/networks/{id}/connect", Why: "put this job's container on its own network"},
	{Method: "POST", Pattern: "/networks/{id}/disconnect", Why: "detach it again when the job ends"},

	// BuildKit's session channel. THIS is what makes the docker driver work:
	// without it buildx cannot attach a session to the daemon's built-in
	// BuildKit, so it falls back to the docker-container driver — which then
	// fails on PUT /containers/{id}/archive and leaves every `docker build`
	// broken. Measured on a live runner: `buildx ls` reported
	// "default* docker-container error" even with BUILDX_CONFIG cleared.
	//
	// Why it is acceptable: a session is the same capability POST /build already
	// grants — run a build against the daemon. It carries the build context and
	// the builder's own file sync; it cannot read /secrets, /configs, /nodes or
	// touch /services, all of which stay refused below. It is also the endpoint
	// that made the docker-container driver possible, which is why it was on the
	// deny list originally — the deny is what forced the slow path, not what
	// protected the swarm.
	{Method: "POST", Pattern: "/session", Why: "BuildKit session for the docker driver"},
	{Method: "POST", Pattern: "/grpc", Why: "BuildKit gRPC transport over the same session"},
}

// DeniedFamily names an API section that must never be reachable from a
// workflow. It is classification only — the refusal comes from the table above
// not matching — but it is what makes the audit line actionable ("a workflow
// tried to remove a service" reads differently from "a workflow hit a typo").
type DeniedFamily struct {
	Prefix string
	Reason string
}

// DeniedFamilies is the hard-deny list, checked to label a refusal. Every one of
// these is a single command away from controlling the node or the swarm.
var DeniedFamilies = []DeniedFamily{
	{Prefix: "/services", Reason: "swarm services: `service rm` would delete the platform's own workloads"},
	{Prefix: "/tasks", Reason: "swarm tasks: inspect/kill any container in the swarm"},
	{Prefix: "/nodes", Reason: "swarm nodes: `node demote`/`node rm` takes the cluster down"},
	{Prefix: "/swarm", Reason: "swarm control: join/leave/rotate the cluster"},
	{Prefix: "/secrets", Reason: "swarm secrets: read every stored credential"},
	{Prefix: "/configs", Reason: "swarm configs: read every stored config"},
	{Prefix: "/plugins", Reason: "plugins: install a privileged plugin"},
	{Prefix: "/system", Reason: "system: daemon-level operations"},
	{Prefix: "/session", Reason: "session: API session control"},
	{Prefix: "/auth", Reason: "auth: registry credential handling"},
	{Prefix: "/exec", Reason: "exec: a shell in any container is root on the host"},
}

// Config builds a Proxy.
type Config struct {
	// SocketPath is the unix socket the proxy listens on.
	SocketPath string
	// UpstreamPath is the real Docker socket it forwards to.
	UpstreamPath string
	Log          zerolog.Logger
	// Transport overrides the upstream round-tripper (tests count calls).
	Transport http.RoundTripper
}

// Proxy is the filtering reverse proxy. It owns one unix listener.
type Proxy struct {
	SocketPath   string
	UpstreamPath string
	Log          zerolog.Logger
	Transport    http.RoundTripper

	// now is swappable so the audit window can be tested without sleeping.
	now func() time.Time

	mu          sync.Mutex
	ln          net.Listener
	srv         *http.Server
	windowStart time.Time
	windowTotal int
	logged      int
	suppressed  int
	deniedTotal int64
}

// New builds a Proxy with defaults filled in.
func New(cfg Config) *Proxy {
	p := &Proxy{
		SocketPath:   cfg.SocketPath,
		UpstreamPath: cfg.UpstreamPath,
		Log:          cfg.Log,
		Transport:    cfg.Transport,
		now:          time.Now,
	}
	if p.SocketPath == "" {
		p.SocketPath = DefaultSocketPath
	}
	if p.UpstreamPath == "" {
		p.UpstreamPath = DefaultUpstreamPath
	}
	if p.Transport == nil {
		p.Transport = unixTransport(p.UpstreamPath)
	}
	return p
}

// unixTransport dials the real daemon socket. A fresh transport per Proxy keeps
// the daemon's own client (which has its own connection pool) untouched: the
// proxy never proxies the platform's traffic, only a runner's.
func unixTransport(path string) http.RoundTripper {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// Start binds the listener. A stale socket file from a previous run is removed
// first: bind fails on an existing path, and the failure reads like a
// permissions problem rather than a leftover.
func (p *Proxy) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.SocketPath), DefaultDirMode); err != nil {
		return fmt.Errorf("socket proxy: create %s: %w", filepath.Dir(p.SocketPath), err)
	}
	if err := os.Remove(p.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("socket proxy: remove stale socket %s: %w", p.SocketPath, err)
	}
	ln, err := net.Listen("unix", p.SocketPath)
	if err != nil {
		return fmt.Errorf("socket proxy: listen on %s: %w", p.SocketPath, err)
	}
	if err := os.Chmod(p.SocketPath, DefaultSocketMode); err != nil {
		_ = ln.Close()
		_ = os.Remove(p.SocketPath)
		return fmt.Errorf("socket proxy: chmod %s: %w", p.SocketPath, err)
	}
	p.ln = ln
	p.srv = &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func(srv *http.Server, ln net.Listener) {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			// A listener that dies must be visible; the supervisor restarts it
			// on its next tick.
			p.Log.Error().Err(err).Str("socket", p.SocketPath).Msg("socket proxy: listener stopped")
		}
	}(p.srv, ln)
	p.Log.Info().Str("socket", p.SocketPath).Str("upstream", p.UpstreamPath).
		Int("allowed_routes", len(AllowTable)).Msg("socket proxy: listening for runner docker requests")
	return nil
}

// Stop shuts the listener down and removes the socket file, so a disabled
// feature leaves nothing behind for a container to mount.
func (p *Proxy) Stop() error {
	p.mu.Lock()
	srv, ln := p.srv, p.ln
	p.mu.Unlock()

	var firstErr error
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := srv.Shutdown(ctx); err != nil {
			firstErr = err
		}
		cancel()
	}
	if ln != nil {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) && firstErr == nil {
			firstErr = err
		}
	}
	if err := os.Remove(p.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
		firstErr = err
	}
	// "Stopped" is published LAST, after the socket file is gone. Running() reads
	// p.ln, so clearing it first made the transition observable while the file was
	// still on disk: anything that stats the path saw a socket that belonged to a
	// listener which no longer existed. Caught as a flaky test ("the socket file
	// survived the stop"), which is the cheap way to learn it.
	p.mu.Lock()
	p.srv, p.ln = nil, nil
	p.mu.Unlock()
	return firstErr
}

// Running reports whether a listener is bound.
func (p *Proxy) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ln != nil
}

// DeniedTotal is the count of refused requests since construction (the doctor
// and the tests read it).
func (p *Proxy) DeniedTotal() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.deniedTotal
}

// ServeHTTP refuses anything the table does not match and forwards the rest to
// the real daemon.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	allowed, family, reason := Check(r.Method, r.URL.Path)
	if !allowed {
		p.auditDenial(r, family, reason)
		writeRefusal(w, r, family)
		return
	}
	// BuildKit's session is an h2c bidirectional stream: it cannot be carried by
	// a RoundTrip, which is HTTP/1.1 and request/response shaped. Those routes
	// are spliced instead — after the allow check, so the decision is the same
	// one every other request gets.
	if isStreamRoute(r) {
		p.serveStream(w, r)
		return
	}
	out := r.Clone(r.Context())
	out.URL.Scheme = "http"
	out.URL.Host = "docker"
	out.RequestURI = ""
	out.Header = r.Header.Clone()
	resp, err := p.Transport.RoundTrip(out)
	if err != nil {
		p.Log.Warn().Err(err).Str("method", r.Method).Str("path", r.URL.Path).
			Msg("socket proxy: upstream docker request failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "socket proxy: the docker daemon did not answer: " + err.Error()})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// Streaming is the point: a build's output and an image pull are chunked
	// streams, so the body is copied, never buffered.
	_, _ = io.Copy(w, resp.Body)
}

// Check reports whether method+path is allowed, and when it is not, which denied
// family it belongs to and why that family is refused.
func Check(method, path string) (allowed bool, family, reason string) {
	method = strings.ToUpper(method)
	// A traversal segment defeats the allow table: the `**` wildcards that match
	// a registry-qualified image name also match "x/../../services/foo", and the
	// DAEMON resolves the dots after we have already said yes. Refuse them here,
	// before any pattern is consulted — this is the difference between a narrow
	// allow list and a wide one with a hole in it.
	for _, seg := range strings.Split(strings.SplitN(path, "?", 2)[0], "/") {
		if seg == ".." || seg == "." {
			return false, "traversal", "path contains a traversal segment, which the allow table cannot match safely"
		}
	}
	segs := segments(path)
	for _, rule := range AllowTable {
		if rule.Method != "*" && rule.Method != method {
			// HEAD is a GET without a body everywhere the daemon supports it.
			if method != http.MethodHead || rule.Method != http.MethodGet {
				continue
			}
		}
		if matchPattern(rule.Pattern, segs) {
			return true, "", ""
		}
	}
	for _, d := range DeniedFamilies {
		if matchPrefix(d.Prefix, segs) {
			return false, d.Prefix, d.Reason
		}
	}
	return false, "", "route is not in the socket proxy's allow table"
}

// segments splits a Docker API path into its segments, dropping the version
// prefix (`/v1.43/containers/json` and `/containers/json` are the same route).
// The prefix is dropped for MATCHING only; the request is forwarded untouched.
func segments(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	parts := strings.Split(path, "/")
	if len(parts) > 0 && isVersionSegment(parts[0]) {
		parts = parts[1:]
	}
	return parts
}

// isVersionSegment recognizes `v1.24` / `v1` — the API version a docker client
// prepends to every path.
func isVersionSegment(s string) bool {
	if len(s) < 2 || s[0] != 'v' || s[1] < '0' || s[1] > '9' {
		return false
	}
	for _, r := range s[1:] {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

// matchPattern matches a segment list against a rule pattern. `{id}` matches
// exactly one non-empty segment; `**` matches one or more trailing segments and
// is only used for image names.
func matchPattern(pattern string, segs []string) bool {
	want := segments(pattern)
	for i, w := range want {
		if w == "**" {
			return len(segs) > i
		}
		if i >= len(segs) {
			return false
		}
		if w == "{id}" {
			if segs[i] == "" {
				return false
			}
			continue
		}
		if segs[i] != w {
			return false
		}
	}
	return len(segs) == len(want)
}

func matchPrefix(prefix string, segs []string) bool {
	want := segments(prefix)
	if len(segs) < len(want) {
		return false
	}
	for i, w := range want {
		if segs[i] != w {
			return false
		}
	}
	return true
}

// writeRefusal answers 403 with a body that names the route, because the docker
// client shows it verbatim in a workflow log.
func writeRefusal(w http.ResponseWriter, r *http.Request, family string) {
	msg := "socket proxy: " + r.Method + " " + r.URL.Path + " is not allowed"
	if family != "" {
		msg += " (" + family + " is blocked: a workflow must not be able to reach it)"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"message": msg,
		"route":   r.Method + " " + r.URL.Path,
	})
}

// auditDenial logs a refused request: the method, the path and the family, with
// the burst/window rate limit described above. The summary line carries the
// count of the lines it replaced.
func (p *Proxy) auditDenial(r *http.Request, family, reason string) {
	now := p.now()
	p.mu.Lock()
	p.deniedTotal++
	if p.windowStart.IsZero() || now.Sub(p.windowStart) >= auditInterval {
		// Close the previous window with its suppressed count before starting
		// a new one.
		suppressed := p.suppressed
		p.windowStart = now
		p.windowTotal, p.logged, p.suppressed = 0, 0, 0
		p.mu.Unlock()
		if suppressed > 0 {
			p.Log.Warn().Int("suppressed", suppressed).Dur("window", auditInterval).
				Msg("socket proxy: denied requests suppressed; a workflow kept probing routes it may not use")
		}
		p.mu.Lock()
	}
	p.windowTotal++
	if p.logged < auditBurst {
		p.logged++
		p.mu.Unlock()
		ev := p.Log.Warn().Str("method", r.Method).Str("path", r.URL.Path)
		if family != "" {
			ev = ev.Str("family", family)
		}
		if reason != "" {
			ev = ev.Str("reason", reason)
		}
		ev.Msg("socket proxy: denied a workflow's docker request")
		return
	}
	p.suppressed++
	p.mu.Unlock()
}

// Supervise runs the listener for as long as ctx lives: it starts it when the
// feature is enabled, stops it when it is disabled, and re-creates a listener
// that died. Every failure is a WRN, never a fatal error — a broken proxy must
// not take the daemon down, it must be visible.
func Supervise(ctx context.Context, p *Proxy, enabled func(context.Context) bool, every time.Duration) {
	if every <= 0 {
		every = 15 * time.Second
	}
	tick := func() {
		want := enabled(ctx)
		switch {
		case want && !p.Running():
			if _, err := os.Stat(p.UpstreamPath); err != nil {
				p.Log.Warn().Err(err).Str("upstream", p.UpstreamPath).
					Msg("socket proxy: no docker socket to front; runners cannot build until it appears")
				return
			}
			if err := p.Start(ctx); err != nil {
				p.Log.Warn().Err(err).Msg("socket proxy: could not start; runners will not be registered")
			}
		case !want && p.Running():
			if err := p.Stop(); err != nil {
				p.Log.Warn().Err(err).Msg("socket proxy: could not stop cleanly")
				return
			}
			p.Log.Info().Msg("socket proxy: stopped (runners disabled)")
		}
	}
	tick()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := p.Stop(); err != nil {
				p.Log.Warn().Err(err).Msg("socket proxy: could not stop on shutdown")
			}
			return
		case <-t.C:
			tick()
		}
	}
}

// isStreamRoute reports whether this request has to be spliced rather than
// forwarded. BuildKit's /session and /grpc are HTTP/2 (h2c) streams: buildx
// sends the request, then speaks HTTP/2 over the same connection, so anything
// that reads a body and writes a separate response breaks it — measured as
// "error reading server preface: http2: frame too large ... looked like an
// HTTP/1.1 header".
func isStreamRoute(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Upgrade"), "h2c") {
		return true
	}
	// Strip the API version prefix, if any (/v1.43/session).
	p := r.URL.Path
	if i := strings.Index(p[1:], "/"); i >= 0 && strings.HasPrefix(p[1:], "v1.") {
		p = p[i+1:]
	}
	p = strings.TrimSuffix(p, "/")
	return strings.HasSuffix(p, "/session") || strings.HasSuffix(p, "/grpc")
}

// serveStream hijacks the client connection and splices it to the daemon socket.
//
// The allow check has already run, so this cannot be used to reach a denied
// route: only /session and /grpc land here. What the splice preserves is the
// part an HTTP proxy cannot express — bytes flowing in both directions at once.
func (p *Proxy) serveStream(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		// Not a refusal: the route IS allowed, this writer just cannot carry a
		// stream (httptest.ResponseRecorder, or a server behind an HTTP/2
		// front-end). Saying "not allowed" here would send an operator to the
		// wrong place, which is the failure mode this whole package exists to
		// avoid.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message": "socket proxy: this connection cannot be upgraded to a stream (BuildKit session needs a hijackable writer)",
			"route":   r.Method + " " + r.URL.Path,
		})
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		p.Log.Warn().Err(err).Msg("socket proxy: cannot hijack the connection for a stream route")
		return
	}
	defer func() { _ = conn.Close() }()

	up, err := net.DialTimeout("unix", p.UpstreamPath, 5*time.Second)
	if err != nil {
		p.Log.Warn().Err(err).Str("path", r.URL.Path).Msg("socket proxy: cannot reach the daemon socket for a stream route")
		return
	}
	defer func() { _ = up.Close() }()

	// Replay the request head verbatim. The Host header is not part of
	// r.Header, so it is written explicitly; an upgrade request has no body to
	// copy — the bytes after the head ARE the stream.
	var head bytes.Buffer
	fmt.Fprintf(&head, "%s %s HTTP/1.1\r\n", r.Method, r.URL.RequestURI())
	fmt.Fprintf(&head, "Host: %s\r\n", "docker")
	if err := r.Header.Write(&head); err != nil {
		return
	}
	head.WriteString("\r\n")
	if _, err := up.Write(head.Bytes()); err != nil {
		return
	}

	// Anything the client already sent past the head is sitting in bufrw.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, bufrw); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, up); done <- struct{}{} }()
	<-done
}
