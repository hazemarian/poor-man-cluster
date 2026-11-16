package cli

import (
	"context"
	"sync"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// leaderFake is the same fake noderuntime's own tests use: the CLI asks the same
// leader questions through the same interface, and a test double per package is
// cheaper than a cross-package test import.
type leaderFake struct {
	runtime.Client
	nodes   []runtime.Node
	nodeErr error
	info    runtime.Info
	infoErr error
}

func (f *leaderFake) NodeList(context.Context) ([]runtime.Node, error) {
	return f.nodes, f.nodeErr
}

// Info defaults to "not a swarm worker" (inactive) so existing standalone/
// leader tests keep today's behaviour; the worker test overrides it.
func (f *leaderFake) Info(context.Context) (runtime.Info, error) {
	return f.info, f.infoErr
}

// leaderFakeMutex is leaderFake with a mutex so a test can flip leadership
// while WatchSwarmLeadership's poll goroutine is reading the node list.
type leaderFakeMutex struct {
	runtime.Client
	mu    sync.Mutex
	nodes []runtime.Node
}

func (f *leaderFakeMutex) NodeList(context.Context) ([]runtime.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtime.Node(nil), f.nodes...), nil
}

// Info is unused by the mutex fake's tests; report a non-worker.
func (f *leaderFakeMutex) Info(context.Context) (runtime.Info, error) {
	return runtime.Info{}, nil
}

func (f *leaderFakeMutex) setLeader(hostname string, leader bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.nodes {
		if f.nodes[i].Hostname == hostname {
			f.nodes[i].IsLeader = leader
		}
	}
}
