package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func TestObserveTokenPreservesIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := auth.GenerateToken()
	if err = ensureObserveToken(context.Background(), st, token); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = ensureObserveToken(context.Background(), st, token); err != nil {
		t.Fatal(err)
	}
	other, _ := auth.GenerateToken()
	if err = ensureObserveToken(context.Background(), st, other); err == nil {
		t.Fatal("replacement token accepted")
	}
	count, _ := st.CountUsers(context.Background())
	if count != 1 {
		t.Fatalf("users=%d", count)
	}
}

type observeRuntimeFake struct {
	runtime.Client
	info runtime.Info
	err  error
}

func (f observeRuntimeFake) Info(context.Context) (runtime.Info, error) {
	return f.info, f.err
}

func TestStandaloneObserverRuntime_ProjectsHealthyLocalHost(t *testing.T) {
	d := standaloneObserverRuntime{Client: observeRuntimeFake{info: runtime.Info{
		Name: "deshli", ServerVersion: "29.1.3",
	}}}
	nodes, err := d.NodeList(context.Background())
	if err != nil {
		t.Fatalf("NodeList: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("NodeList returned %d rows, want one", len(nodes))
	}
	n := nodes[0]
	if n.Hostname != "deshli" || n.Role != "standalone" || n.Status != "ready" || n.Availability != "unknown" {
		t.Fatalf("node = %+v, want local standalone ready/active row", n)
	}
	if n.IsLeader || n.ID != "standalone:deshli" || n.EngineVersion != "29.1.3" {
		t.Fatalf("node swarm fields = %+v, want no leader and stable standalone identity", n)
	}
}

func TestStandaloneObserverRuntime_PropagatesDockerInfoFailure(t *testing.T) {
	want := context.Canceled
	d := standaloneObserverRuntime{Client: observeRuntimeFake{err: want}}
	if _, err := d.NodeList(context.Background()); err != want {
		t.Fatalf("NodeList error = %v, want %v", err, want)
	}
}
