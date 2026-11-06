// Package nodever publishes each manager's pmcluster binary version into the
// swarm as a Docker config, so version skew across a fleet is visible instead
// of being discovered by whoever next runs the upgrade script.
//
// Why a config and not a node label: labels and configs have the same write
// restriction (a manager), and a config carries data plus labels without
// pretending to be placement metadata. Why not a heartbeat to the leader: the
// leader is exactly the node whose availability you cannot assume.
//
// Workers are excluded BY THE PLATFORM, not by this package: a worker cannot
// create a config or set a node label, so a worker's version is unknowable from
// the swarm. That is a real limit, stated here because a version list that
// silently omitted workers would read as "those are current".
package nodever

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

const (
	// NamePrefix prefixes every version config.
	NamePrefix = "pmcluster_nodever"
	// LabelKind marks the family, so a list query is one call.
	LabelKind = "pmcluster.kind"
	// Kind is the label value for version configs.
	Kind = "node-version"
	// LabelHostnames carries the reporting hostname.
	LabelHostname = "pmcluster.hostname"
	// LabelVersion carries the binary version.
	LabelVersion = "pmcluster.binary-version"
)

// Name is the config name for one host.
func Name(hostname string) string {
	return NamePrefix + "_" + sanitize(hostname)
}

// sanitize keeps Docker's config-name character set.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// Publisher writes this node's version on a timer.
type Publisher struct {
	Docker   runtime.Client
	Hostname string
	Version  string
	Log      zerolog.Logger
	// Interval between publishes; 0 means 10 minutes.
	Interval time.Duration
}

// Publish writes the config unless it already carries this version. Returns
// whether it wrote.
func (p *Publisher) Publish(ctx context.Context) (bool, error) {
	if p.Docker == nil || p.Hostname == "" || p.Version == "" {
		return false, nil
	}
	// Asked on EVERY tick, not once at startup. A node that restarts during an
	// upgrade reaches this code before its swarm membership is back, and a
	// one-shot probe then decided "not a manager" for the life of the process —
	// seen live: two of three managers never published.
	if info, err := p.Docker.Info(ctx); err != nil || !info.SwarmControlAvailable {
		return false, nil
	}
	name := Name(p.Hostname)
	if existing, err := p.Docker.ConfigInspect(ctx, name); err == nil {
		if existing.Labels[LabelVersion] == p.Version {
			return false, nil
		}
	}
	err := p.Docker.ConfigCreate(ctx, runtime.ConfigSpec{
		Name: name,
		Data: []byte(p.Version),
		Labels: map[string]string{
			LabelKind:     Kind,
			LabelHostname: p.Hostname,
			LabelVersion:  p.Version,
			// Same key the managed configs carry, so the existing drift check
			// and this one agree on what "the version" means.
			"pmcluster.version": p.Version,
		},
	})
	if err != nil {
		return false, fmt.Errorf("publish node version config %s: %w", name, err)
	}
	return true, nil
}

// Loop publishes immediately and then on the interval, until ctx is done.
// Errors are logged at most once per distinct message: a manager that cannot
// write configs (a worker, or a leaderless swarm) would otherwise log every tick.
func (p *Publisher) Loop(ctx context.Context) {
	interval := p.Interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	var lastErr string
	tick := func() {
		if _, err := p.Publish(ctx); err != nil && err.Error() != lastErr {
			lastErr = err.Error()
			p.Log.Warn().Err(err).Msg("could not publish this node's binary version")
			return
		}
		lastErr = ""
	}
	tick()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// Entry is one node's reported version.
type Entry struct {
	Hostname string `json:"hostname"`
	Version  string `json:"version"`
}

// Skew is what the fleet reports against one reference version.
type Skew struct {
	Reference string  `json:"reference"`
	Nodes     []Entry `json:"nodes"`
	Stale     []Entry `json:"stale,omitempty"`
	Unknown   int     `json:"unknown"`
}

// OK reports whether every reporting node matches the reference.
func (s Skew) OK() bool { return len(s.Stale) == 0 }

// Read lists every published version and compares it to reference. Nodes that
// never published are counted, not guessed.
func Read(ctx context.Context, dc runtime.Client, reference string) (Skew, error) {
	if dc == nil {
		return Skew{}, fmt.Errorf("no swarm access")
	}
	names, err := dc.ConfigList(ctx, LabelKind, Kind)
	if err != nil {
		return Skew{}, fmt.Errorf("list node-version configs: %w", err)
	}
	out := Skew{Reference: reference}
	for _, name := range names {
		c, err := dc.ConfigInspect(ctx, name)
		if err != nil {
			continue
		}
		out.Nodes = append(out.Nodes, Entry{
			Hostname: c.Labels[LabelHostname],
			Version:  c.Labels[LabelVersion],
		})
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].Hostname < out.Nodes[j].Hostname })
	for _, e := range out.Nodes {
		if reference != "" && e.Version != reference {
			out.Stale = append(out.Stale, e)
		}
	}
	return out, nil
}
