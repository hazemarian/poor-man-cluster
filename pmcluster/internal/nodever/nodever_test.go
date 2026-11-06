package nodever

import (
	"context"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// fakeClient implements only the two config methods the package uses; the
// embedded nil interface makes any other call panic instead of lying.
type fakeClient struct {
	runtime.Client
	configs map[string]runtime.ConfigInspectResult
	creates int
	// control is what Info reports: false models a worker (or a node whose
	// swarm membership is not back yet).
	control bool
}

func (f *fakeClient) Info(context.Context) (runtime.Info, error) {
	return runtime.Info{SwarmControlAvailable: f.control}, nil
}

func (f *fakeClient) ConfigList(_ context.Context, key, value string) ([]string, error) {
	var out []string
	for name, c := range f.configs {
		if key != "" && c.Labels[key] != value {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

func (f *fakeClient) ConfigInspect(_ context.Context, name string) (runtime.ConfigInspectResult, error) {
	c, ok := f.configs[name]
	if !ok {
		return runtime.ConfigInspectResult{}, context.Canceled
	}
	return c, nil
}

func (f *fakeClient) ConfigCreate(_ context.Context, spec runtime.ConfigSpec) error {
	f.creates++
	f.configs[spec.Name] = runtime.ConfigInspectResult{Labels: spec.Labels, Data: spec.Data}
	return nil
}

// Publishing twice must write once: the loop ticks every ten minutes and a
// needless config rewrite is churn in Raft for nothing.
func TestPublishIsIdempotentForTheSameVersion(t *testing.T) {
	ctx := context.Background()
	f := &fakeClient{configs: map[string]runtime.ConfigInspectResult{}, control: true}
	p := &Publisher{Docker: f, Hostname: "nxt-sw-1-m", Version: "v0.2.241"}

	wrote, err := p.Publish(ctx)
	if err != nil || !wrote {
		t.Fatalf("first publish: wrote=%v err=%v", wrote, err)
	}
	wrote, err = p.Publish(ctx)
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if wrote || f.creates != 1 {
		t.Fatalf("re-published an unchanged version (writes=%d)", f.creates)
	}
}

// A new binary version must be published, and the labels must carry both the
// hostname and the version so a reader needs one call per node.
func TestPublishRewritesOnANewVersion(t *testing.T) {
	ctx := context.Background()
	f := &fakeClient{configs: map[string]runtime.ConfigInspectResult{}, control: true}
	p := &Publisher{Docker: f, Hostname: "nxt-sw-4-m", Version: "v0.2.240"}
	if _, err := p.Publish(ctx); err != nil {
		t.Fatalf("publish: %v", err)
	}
	p.Version = "v0.2.241"
	if wrote, err := p.Publish(ctx); err != nil || !wrote {
		t.Fatalf("re-publish after upgrade: wrote=%v err=%v", wrote, err)
	}
	got := f.configs[Name("nxt-sw-4-m")].Labels
	if got[LabelVersion] != "v0.2.241" || got[LabelHostname] != "nxt-sw-4-m" {
		t.Errorf("labels = %v", got)
	}
}

// Skew is the whole point: a node behind the reference has to be named.
func TestReadNamesTheStragglers(t *testing.T) {
	ctx := context.Background()
	f := &fakeClient{configs: map[string]runtime.ConfigInspectResult{}, control: true}
	for _, n := range []struct{ host, ver string }{
		{"nxt-sw-1-m", "v0.2.241"},
		{"nxt-sw-4-m", "v0.2.241"},
		{"nxt-sw-5-m", "v0.2.238"},
	} {
		p := &Publisher{Docker: f, Hostname: n.host, Version: n.ver}
		if _, err := p.Publish(ctx); err != nil {
			t.Fatalf("publish %s: %v", n.host, err)
		}
	}

	skew, err := Read(ctx, f, "v0.2.241")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(skew.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(skew.Nodes))
	}
	if skew.OK() || len(skew.Stale) != 1 || skew.Stale[0].Hostname != "nxt-sw-5-m" {
		t.Fatalf("stale = %#v, want only nxt-sw-5-m", skew.Stale)
	}
}

// A node that restarts during an upgrade reaches the publish path before its
// swarm membership is back. Deciding "not a manager" once, at that moment, is
// how two of three managers never published: the check has to happen per tick.
func TestPublishSkipsWhileTheSwarmIsNotReadyAndRetries(t *testing.T) {
	ctx := context.Background()
	f := &fakeClient{configs: map[string]runtime.ConfigInspectResult{}}
	p := &Publisher{Docker: f, Hostname: "nxt-sw-1-m", Version: "v0.2.242"}

	if wrote, err := p.Publish(ctx); err != nil || wrote {
		t.Fatalf("published before the node was a manager: wrote=%v err=%v", wrote, err)
	}
	f.control = true
	if wrote, err := p.Publish(ctx); err != nil || !wrote {
		t.Fatalf("did not publish once the swarm answered: wrote=%v err=%v", wrote, err)
	}
}
