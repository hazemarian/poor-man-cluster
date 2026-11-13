package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/clustersnapshot"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runners"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// The node-local report.
//
// A WORKER used to get no doctor at all: the command is leader-scoped because the
// secrets, the last-apply record and the config index come from the local store,
// and refusing is right for those. But "the cluster's secrets do not decrypt
// here" is not the same as "nothing can be measured here", and the operator was
// left with no way to ask a worker why its runner never started — which is
// exactly the question that stayed unanswered for a whole release.
//
// So a non-leader gets an honest subset: what THIS machine can answer, plus one line
// naming each thing it cannot (and where to ask instead).
//
// Since ADR 0003's slice 3 it also answers the cluster questions the follower MIRRORS:
// the settings/secrets/users breadth, whose leader it came from, and how long ago it was
// applied. That is a deliberate change of the old rule — the report used to refuse the
// store entirely to avoid answering from a stale copy. Labelling the answer with its age
// is what makes reading it safe: a stale answer an operator can see is worth more than a
// blank one, and a blank one is what sent people to the wrong node.

// NodeInput is everything the node-local checks read.
type NodeInput struct {
	// Hostname is this node's swarm hostname.
	Hostname string
	// ListenAddr is the address the daemon API binds on this node.
	ListenAddr string
	// SocketPath is the filtered docker socket path this node intends to serve.
	SocketPath string
	// StatePath is where the runner agent records the assignment it applied.
	StatePath string
	// VolumeRoot is where stack data lives (free-space check).
	VolumeRoot string
	// Docker is the local container runtime (runner container state).
	Docker runners.LocalDocker
	// Health probes one URL and returns (status, error). Nil uses a 2s HTTP GET.
	Health func(ctx context.Context, url string) (int, error)
	// MemTotalMiB is this machine's total RAM, so the runner's memory cap can be
	// compared against the machine that would have to survive it. 0 means unknown
	// (the check is then skipped, never guessed).
	MemTotalMiB int64
	// Store is this node's control-plane database, which on a follower manager holds the
	// MIRROR of the leader's settings/secrets/users (ADR 0003, slice 3). Nil on a worker:
	// a worker has no store, and a missing store is reported as such rather than opening
	// one, which would seed an empty database whose answers look plausible.
	Store *store.Store
	// Role is "manager" or "worker", from the live swarm node spec. It decides what the
	// swarm line can honestly claim: a follower MANAGER can read the node list, a worker
	// cannot.
	Role string
	// Now is the clock (tests). It also ages the mirror.
	Now time.Time
}

// RunNodeLocal produces the worker's report: four measurements it can make, the
// free-space check every node can make, and a skipped line per cluster question
// that only the leader can answer.
func RunNodeLocal(ctx context.Context, in NodeInput) *Report {
	rep := &Report{At: time.Now().UTC()}
	add := func(c Check) { rep.Checks = append(rep.Checks, c) }

	add(checkLocalDaemon(ctx, in))
	add(checkLocalAssignment(in))
	add(checkLocalRunner(ctx, in))
	add(checkLocalRunnerMemory(in))
	add(checkRunnerCaches(ctx, in))
	add(checkDisk(Input{VolumeRoot: in.VolumeRoot}))
	add(checkSwarmNodeLocal(ctx, in))
	add(checkMirrorState(ctx, in))
	add(Check{
		Name:   "cluster state",
		Status: StatusSkip,
		Detail: "not mirrored, so not answered here: secret DECRYPTION (the cipher key is provisioned at join, never carried by the snapshot), stacks and their revisions, the last apply, and the control-plane kit drill. Run `pmcluster doctor` on the leader, or from your own machine with --api-url",
	})

	rep.OK = true
	for _, c := range rep.Checks {
		if c.Status == StatusFail {
			rep.OK = false
		}
	}
	return rep
}

// checkLocalDaemon answers "is this node's daemon listening?". It is the check
// that would have caught the whole v0.2.267 defect in one line: a worker's daemon
// stood by without ever binding, so nothing listened on 9090 at all.
func checkLocalDaemon(ctx context.Context, in NodeInput) Check {
	addr := in.ListenAddr
	if strings.TrimSpace(addr) == "" {
		addr = "127.0.0.1:9090"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return Check{"daemon", StatusWarn, "listen address is not host:port: " + addr}
	}
	// A worker binds loopback, so the probe is loopback whatever the configured
	// host was; probing 0.0.0.0 would be a dial to the unspecified address.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/health"

	health := in.Health
	if health == nil {
		health = func(ctx context.Context, url string) (int, error) {
			c, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(c, http.MethodGet, url, nil)
			if err != nil {
				return 0, err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return 0, err
			}
			defer func() { _ = resp.Body.Close() }()
			return resp.StatusCode, nil
		}
	}
	code, err := health(ctx, url)
	switch {
	case err != nil:
		return Check{"daemon", StatusFail, fmt.Sprintf("nothing listening at %s (%v) — `systemctl status pmcluster`", url, err)}
	case code >= 500:
		return Check{"daemon", StatusFail, fmt.Sprintf("%s answered %d", url, code)}
	case code != http.StatusOK:
		return Check{"daemon", StatusWarn, fmt.Sprintf("%s answered %d", url, code)}
	}
	return Check{"daemon", StatusOK, "answering /health at " + url}
}

// checkLocalAssignment answers "when did this node last learn what to run, and
// from where?" — the question a worker could not answer at all before.
func checkLocalAssignment(in NodeInput) Check {
	if strings.TrimSpace(in.StatePath) == "" {
		return Check{"runner assignment", StatusSkip, "no state path on this node"}
	}
	rec, ok := readAgentRecord(in.StatePath)
	if !ok {
		return Check{"runner assignment", StatusSkip, "never fetched (no " + filepath.Base(in.StatePath) + "): this node has not applied a runner assignment yet — check the daemon log for the swarm-certificate warning"}
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	age := "unknown age"
	if rec.FetchedAt > 0 {
		age = time.Since(time.Unix(rec.FetchedAt, 0)).Round(time.Second).String() + " ago"
	}
	source := rec.Source
	if source == "" {
		source = "unknown source"
	}
	detail := fmt.Sprintf("fetched %s from %s: enabled=%t", age, source, rec.Enabled)
	if rec.Enabled {
		detail += fmt.Sprintf(", org=%s, runner=%s", orDash(rec.Org), orDash(rec.RunnerName))
	}
	if rec.LastError != "" {
		return Check{"runner assignment", StatusWarn, detail + "; last apply error: " + rec.LastError}
	}
	// An assignment older than ~3 polls means the leader stopped answering.
	if rec.FetchedAt > 0 && now.Sub(time.Unix(rec.FetchedAt, 0)) > 5*time.Minute {
		return Check{"runner assignment", StatusWarn, detail + " (stale: the leader has not answered for a while)"}
	}
	return Check{"runner assignment", StatusOK, detail}
}

// checkLocalRunner reports this node's own runner container.
func checkLocalRunner(ctx context.Context, in NodeInput) Check {
	if in.Docker == nil {
		return Check{"local runner", StatusSkip, "no docker client on this node"}
	}
	name := runners.RunnerName(in.Hostname)
	c, ok, err := in.Docker.Inspect(ctx, name)
	switch {
	case err != nil:
		return Check{"local runner", StatusWarn, fmt.Sprintf("could not inspect %s: %v", name, err)}
	case !ok:
		return Check{"local runner", StatusOK, "no runner container on this node (" + name + ")"}
	case c.Running():
		exposure := "filtered socket"
		if c.HasRawSocketMount() {
			exposure = "RAW /var/run/docker.sock — the socket proxy is bypassed"
			return Check{"local runner", StatusWarn, fmt.Sprintf("%s is running with the %s", name, exposure)}
		}
		return Check{"local runner", StatusOK, fmt.Sprintf("%s is running with the %s", name, exposure)}
	}
	return Check{"local runner", StatusWarn, fmt.Sprintf("%s exists but is not running (state %q)", name, c.State)}
}

// checkLocalRunnerMemory answers the question that would have saved nxt-sw-3-w:
// does this node's configured runner cap fit THIS machine?
//
// A worker has no store, so the cap comes from the assignment it last applied
// (AgentRecord.Memory) and the size from its own /proc/meminfo. That is the only
// place this comparison can be made on the node that would be OOM-killed, and a
// refusal is a FAIL: the runner will not start, and the reason names both numbers.
func checkLocalRunnerMemory(in NodeInput) Check {
	rec, ok := readAgentRecord(in.StatePath)
	if !ok || strings.TrimSpace(rec.Memory) == "" {
		return Check{"runner memory", StatusSkip, "no runner memory cap recorded on this node yet (no assignment applied)"}
	}
	if in.MemTotalMiB <= 0 {
		return Check{"runner memory", StatusSkip, "could not read this node's MemTotal, so the cap cannot be checked against it"}
	}
	if err := runners.CheckMemoryFit(in.Hostname, in.MemTotalMiB, true, rec.Memory); err != nil {
		return Check{"runner memory", StatusFail, err.Error()}
	}
	return Check{"runner memory", StatusOK,
		fmt.Sprintf("a %s cap fits this node's %d MB (max %d%%)", rec.Memory, in.MemTotalMiB, runners.MemoryCeilingPercent)}
}

// checkMirrorState answers what a follower DOES hold: the leader's settings, secrets and
// tokens, as of an age this node measured itself.
//
// The age is the point of the check. "The mirror is 4 minutes old" is a finding an
// operator can act on; a green tick next to a copy nobody has refreshed since the leader
// died is the failure mode this replaces. Stale is a WARN, not a FAIL: the data is real,
// just old, and the cluster may be mid-failover.
func checkMirrorState(ctx context.Context, in NodeInput) Check {
	// Role first, and before the store: a worker HAS a data.db (join creates it), so a
	// store-based check answers "never synced" and warns about a mirror a worker will never
	// have. Measured on 6-w after the first deploy: "no snapshot applied on this node yet —
	// a follower mirrors within one poll", on a node where the poll loop never starts.
	if strings.EqualFold(strings.TrimSpace(in.Role), "worker") {
		return Check{
			Name:   "cluster mirror",
			Status: StatusSkip,
			Detail: "this node is a worker: it holds no mirror by design. The leader's settings are mirrored on managers, and a worker is told what to run by its runner assignment",
		}
	}
	if in.Store == nil {
		return Check{
			Name:   "cluster mirror",
			Status: StatusSkip,
			Detail: "no control-plane database on this node (a worker has none): the mirror exists on managers only, and a worker is told what to run by its assignment",
		}
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	m, err := in.Store.MirrorReading(ctx, now)
	switch {
	case errors.Is(err, store.ErrNeverSynced):
		// A follower mirrors within one poll of its daemon starting, so this is either a
		// brand-new node or a mirror that has never once reached the leader. Both are
		// worth saying out loud, and neither is the old blank.
		return Check{
			Name:   "cluster mirror",
			Status: StatusWarn,
			Detail: "no snapshot applied on this node yet: a follower mirrors within one poll (60 s) of its daemon starting — check the daemon log for the `cluster mirror` line",
		}
	case err != nil:
		return Check{Name: "cluster mirror", Status: StatusWarn, Detail: "could not read the mirror: " + err.Error()}
	}
	detail := fmt.Sprintf("holds the settings/secrets/users mirror · %s (age_seconds=%d)", m.Human(), m.AgeSeconds)
	if content := m.ContentAge(); content != "" {
		detail += " · " + content
	}
	// The threshold is on CONTACT, not on content: a 304 every minute is a current mirror.
	if m.AgeSeconds > int64(mirrorStaleAfter/time.Second) {
		return Check{
			Name:   "cluster mirror",
			Status: StatusWarn,
			Detail: fmt.Sprintf("%s — STALE: the leader has not ANSWERED for over %s (a poll that reaches it resets this). The values are real but old; the leader it came from was %s", detail, mirrorStaleAfter, m.Leader),
		}
	}
	return Check{Name: "cluster mirror", Status: StatusOK, Detail: detail}
}

// mirrorStaleAfter is three poll intervals. It is derived from the channel's own constant
// so the loop and this threshold cannot drift apart.
const mirrorStaleAfter = 3 * clustersnapshot.PollInterval

// checkSwarmNodeLocal reports what this node can see of the swarm. A follower MANAGER can
// read the node list and quorum; a worker cannot (the call is manager-only). The old line
// told both of them the same thing, which was wrong for half the nodes it printed on.
func checkSwarmNodeLocal(_ context.Context, in NodeInput) Check {
	if strings.EqualFold(strings.TrimSpace(in.Role), "manager") {
		return Check{
			Name:   "swarm",
			Status: StatusSkip,
			Detail: "the node list and quorum are readable from a manager, but the authoritative view — which managers are current, and whether quorum survives another loss — is only current on the leader. Run `pmcluster doctor` there, or with --api-url",
		}
	}
	return Check{
		Name:   "swarm",
		Status: StatusSkip,
		Detail: "this node is a worker: the node list and quorum are manager-only calls, so they are answered on the leader",
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// readAgentRecord reads the runner agent's state file. It is written 0600 by the
// daemon and read by hand here — corrupt or absent is "never fetched", never an
// error that hides the rest of the report.
func readAgentRecord(path string) (runners.AgentRecord, bool) {
	var rec runners.AgentRecord
	if err := readJSON(path, &rec); err != nil {
		return runners.AgentRecord{}, false
	}
	return rec, true
}

// checkRunnerCaches reports the runner's persistent caches against their ceilings.
//
// It reads the numbers from docker itself, on this node, because that is where they are:
// `docker volume inspect` has no size field and a host `du` would have to know where
// docker put the volumes. The check only applies where a runner actually exists — the
// volumes are created with the first job, so "no caches" on a node with no runner is not
// a finding.
//
// WARN, not error: a full cache is a node about to run out of disk, not a cluster that is
// down. It is the only warning available before a job fails with "no space left on
// device" somewhere unrelated to the cache.
func checkRunnerCaches(ctx context.Context, in NodeInput) Check {
	if in.Docker == nil {
		return Check{"runner caches", StatusSkip, "no local container runtime"}
	}
	name := runners.RunnerName(in.Hostname)
	if _, ok, err := in.Docker.Inspect(ctx, name); err != nil || !ok {
		if _, dok, _ := in.Docker.Inspect(ctx, name+"-dind"); err != nil || !dok {
			return Check{"runner caches", StatusSkip, "no runner container on this node"}
		}
	}
	usage, err := runners.ReadCacheUsage(ctx, in.Docker, name)
	if err != nil {
		return Check{"runner caches", StatusWarn, "could not measure the runner caches: " + err.Error()}
	}
	if warn := runners.CacheWarnings(usage); len(warn) > 0 {
		return Check{"runner caches", StatusWarn, strings.Join(warn, "; ")}
	}
	parts := make([]string, 0, len(usage))
	for _, c := range usage {
		parts = append(parts, c.Name+" "+runners.HumanBytes(c.Bytes))
	}
	return Check{"runner caches", StatusOK,
		strings.Join(parts, " · ") + " · total " + runners.HumanBytes(runners.TotalCacheBytes(usage))}
}
