package cli

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// swarmProbeClient stubs only SwarmID, which is all classifyLocalCluster uses.
type swarmProbeClient struct {
	runtime.Client
	id  string
	err error
}

func (c swarmProbeClient) SwarmID(context.Context) (string, error) { return c.id, c.err }

// TestClassifyLocalCluster covers BUG-022: `cluster update` / `cluster reset`
// must not treat a manager whose local store is intentionally empty (joined but
// never promoted) as a fresh box, which sent install.sh's auto-update into the
// interactive setup wizard.
func TestClassifyLocalCluster(t *testing.T) {
	newStore := func(t *testing.T) *store.Store {
		t.Helper()
		st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st
	}

	t.Run("fresh box — no settings, no swarm", func(t *testing.T) {
		st := newStore(t)
		if got := classifyLocalCluster(context.Background(), st, swarmProbeClient{}); got != stateFresh {
			t.Errorf("got %v, want stateFresh", got)
		}
	})

	t.Run("standby manager — no settings, member of a swarm (BUG-022)", func(t *testing.T) {
		st := newStore(t)
		got := classifyLocalCluster(context.Background(), st, swarmProbeClient{id: "lclisp64h0vvrmi14kz2thxjx"})
		if got != stateStandby {
			t.Errorf("got %v, want stateStandby", got)
		}
	})

	t.Run("swarm probe error is treated as not-in-swarm", func(t *testing.T) {
		st := newStore(t)
		got := classifyLocalCluster(context.Background(), st, swarmProbeClient{err: errors.New("Cannot connect to the Docker daemon")})
		if got != stateFresh {
			t.Errorf("got %v, want stateFresh", got)
		}
	})

	t.Run("nil docker client falls back to fresh", func(t *testing.T) {
		st := newStore(t)
		if got := classifyLocalCluster(context.Background(), st, nil); got != stateFresh {
			t.Errorf("got %v, want stateFresh", got)
		}
	})

	t.Run("provisioned store wins over the swarm probe", func(t *testing.T) {
		st := newStore(t)
		if err := st.SetSetting(context.Background(), cluster.SettingDomain(), "example.com"); err != nil {
			t.Fatalf("set domain: %v", err)
		}
		if got := classifyLocalCluster(context.Background(), st, swarmProbeClient{id: "swarm-abc"}); got != stateProvisioned {
			t.Errorf("got %v, want stateProvisioned", got)
		}
	})
}
