package stacks

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/manifest"
)

// PinResolver decides WHERE a stateful service's data lives. It implements
// the storage placement story (Path 1): every service that holds a volume
// must run on a node that will actually keep that data — either the one the
// manifest pins it to explicitly (always wins), or a deterministic assignment
// across the configured storage nodes, or the single platform node.
//
// Precedence for a stateful service with NO explicit placement:
//
//  1. per-stack pin (from `pmcluster stack move`) — the move must stick
//  2. round-robin across storage_nodes (deterministic per stack name)
//  3. platform_node (single-node clusters, today's default)
//
// Stateless services and role-based placements ("manager"/"worker") are
// never rewritten.
type PinResolver struct {
	// mu guards StorageNodes so the daemon's settings hook can refresh the
	// storage_nodes list live while the reconcile loop resolves placements
	// concurrently.
	mu sync.Mutex

	// PlatformNode is the node.hostname stateful services fall back to when
	// no storage_nodes are configured (the platform_node cluster setting).
	PlatformNode string

	// StorageNodes is the ordered set of hostnames stateful services are
	// round-robined across (the storage_nodes cluster setting). Empty keeps
	// the single-platform-node behaviour. Mutate via SetStorageNodes.
	StorageNodes []string

	// StackPin returns the per-stack placement override (a hostname, or ""
	// when the stack has not been individually moved). Nil disables the
	// override.
	StackPin func(ctx context.Context, stackName string) (string, error)
}

// SetStorageNodes live-replaces the storage-node list. Called by the daemon's
// settings hook when storage_nodes changes, so round-robin placement adapts
// without a restart (the rendered hash changes on the next reconcile, which
// redeploys affected stacks).
func (r *PinResolver) SetStorageNodes(nodes []string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.StorageNodes = nodes
}

func (r *PinResolver) storageNodes() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.StorageNodes
}

// ResolvePlacement rewrites the IR in place: every service that holds a
// volume and left placement empty gets the resolved storage node, so the
// writer's default branch emits node.hostname == <resolved>. Explicit
// placements are never touched. A no-op when every service is explicit or
// stateless.
func (r *PinResolver) ResolvePlacement(ctx context.Context, appName string, ir *manifest.IR) error {
	if r == nil {
		return nil
	}
	pin, err := r.stackPin(ctx, appName)
	if err != nil {
		return fmt.Errorf("resolve pin for %s: %w", appName, err)
	}
	for i := range ir.Services {
		s := &ir.Services[i]
		if s.Placement != "" || len(s.Volumes) == 0 {
			continue // explicit placement, or nothing to pin
		}
		ir.Services[i].Placement = r.resolve(appName, pin)
	}
	return nil
}

// resolve returns the winning node for a stateful service with no explicit
// placement: the per-stack pin first, then round-robin across storage nodes,
// then the platform node. "" means "any node".
func (r *PinResolver) resolve(appName, stackPin string) string {
	switch {
	case stackPin != "":
		return stackPin
	case len(r.storageNodes()) > 0:
		return roundRobinNode(appName, r.storageNodes())
	default:
		return r.PlatformNode
	}
}

// stackPin caches the per-stack pin lookup for one resolution pass.
func (r *PinResolver) stackPin(ctx context.Context, appName string) (string, error) {
	if r.StackPin == nil {
		return "", nil
	}
	return r.StackPin(ctx, appName)
}

// StoragePins returns the distinct hostnames the app's stateful services are
// pinned to: an explicit hostname placement wins, an empty placement resolves
// through the same precedence as ResolvePlacement, and role-based placements
// ("manager"/"worker") never pin a specific node. Empty when the app holds no
// node-pinned data — callers treat that as "nothing to pause".
func (r *PinResolver) StoragePins(ctx context.Context, appName string, ir *manifest.IR) ([]string, error) {
	if r == nil {
		return nil, nil
	}
	pin, err := r.stackPin(ctx, appName)
	if err != nil {
		return nil, fmt.Errorf("resolve pin for %s: %w", appName, err)
	}
	seen := map[string]bool{}
	var out []string
	for i := range ir.Services {
		s := &ir.Services[i]
		if len(s.Volumes) == 0 {
			continue
		}
		node := s.Placement
		if node == "" {
			node = r.resolve(appName, pin)
		}
		if node == "" || node == "manager" || node == "worker" {
			continue
		}
		if !seen[node] {
			seen[node] = true
			out = append(out, node)
		}
	}
	return out, nil
}

// roundRobinNode deterministically assigns a storage node to a stack name —
// the same stack always gets the same node, and adding stacks never reshuffles
// the existing assignments. "Round robin" because distinct names spread
// evenly across the storage nodes.
func roundRobinNode(appName string, nodes []string) string {
	if len(nodes) == 0 {
		return ""
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(appName))
	return nodes[int(h.Sum32())%len(nodes)]
}

// ParseStorageNodes splits a storage_nodes setting value into its distinct,
// trimmed hostnames. "" and whitespace-only entries are dropped.
func ParseStorageNodes(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		node := strings.TrimSpace(part)
		if node == "" || seen[node] {
			continue
		}
		seen[node] = true
		out = append(out, node)
	}
	return out
}

// StackPinKey is the cluster_settings key holding a stack's individual
// placement override after a `stack move`. It is deliberately NOT in the
// settings allowlist — moves write it directly through the store, and the
// settings API cannot edit it.
func StackPinKey(stackName string) string { return "stack_pin_" + stackName }

// resolvePlacements applies the PinResolver to a freshly built IR, when one is
// configured. Nil-safe: a Service without Pins renders exactly as before.
func (s *Service) resolvePlacements(ctx context.Context, appName string, ir *manifest.IR) error {
	if s.Pins == nil {
		return nil
	}
	return s.Pins.ResolvePlacement(ctx, appName, ir)
}

// StoragePinsForStack resolves the storage nodes the stack's latest revision
// pins its stateful services to — used by the reconcile loop to skip syncs
// while a storage node is down. Empty means "nothing node-pinned" (stateless,
// role-constrained, or no resolver configured).
func (s *Service) StoragePinsForStack(ctx context.Context, stackName string) ([]string, error) {
	if s.Pins == nil {
		return nil, nil
	}
	revs, err := s.Store.ListRevisions(ctx, stackName, 1)
	if err != nil {
		return nil, err
	}
	if len(revs) == 0 {
		return nil, nil
	}
	_, built, err := manifest.ParseBuild(ctx, []byte(revs[0].SourceYAML), stackName, s.Resolver)
	if err != nil {
		return nil, fmt.Errorf("re-translate %s: %w", stackName, err)
	}
	return s.Pins.StoragePins(ctx, stackName, built)
}
