package stacks

import (
	"context"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/manifest"
)

// testIR builds a minimal IR with the given services (name, volumes,
// placement).
func testIR(app string, svcs ...IRTestService) *manifest.IR {
	ir := &manifest.IR{Name: app}
	for _, s := range svcs {
		ir.Services = append(ir.Services, manifest.IRService{
			Name:      s.name,
			Image:     "nginx:alpine",
			Volumes:   s.volumes,
			Placement: s.placement,
		})
	}
	return ir
}

// IRTestService is a test fixture row for testIR.
type IRTestService struct {
	name      string
	volumes   []string
	placement string
}

func st(name string, vols []string, placement string) IRTestService {
	return IRTestService{name: name, volumes: vols, placement: placement}
}

func TestParseStorageNodes(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"node-a", []string{"node-a"}},
		{"node-a,node-b", []string{"node-a", "node-b"}},
		{"node-a, node-b ,node-a", []string{"node-a", "node-b"}}, // trim + dedupe
		{"node-a,,node-b", []string{"node-a", "node-b"}},         // empty entry dropped
	}
	for _, c := range cases {
		got := ParseStorageNodes(c.raw)
		if len(got) != len(c.want) {
			t.Fatalf("ParseStorageNodes(%q) = %v, want %v", c.raw, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("ParseStorageNodes(%q) = %v, want %v", c.raw, got, c.want)
			}
		}
	}
}

func TestRoundRobinNode_Deterministic(t *testing.T) {
	nodes := []string{"s1", "s2", "s3"}
	for _, app := range []string{"alpha", "beta", "gamma", "delta"} {
		first := roundRobinNode(app, nodes)
		for i := 0; i < 5; i++ {
			if got := roundRobinNode(app, nodes); got != first {
				t.Fatalf("roundRobinNode(%q) not stable: %s then %s", app, first, got)
			}
		}
	}
	// Distinct names should not all land on the same node (spread check).
	seen := map[string]bool{}
	for _, app := range []string{"alpha", "beta", "gamma", "delta", "eps", "zeta", "eta", "theta"} {
		seen[roundRobinNode(app, nodes)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("round-robin not spreading: all apps on one node %v", seen)
	}
}

func TestResolvePlacement_Precedence(t *testing.T) {
	ctx := context.Background()

	t.Run("StackPin outranks round-robin and platform", func(t *testing.T) {
		r := &PinResolver{
			PlatformNode: "plat",
			StorageNodes: []string{"s1", "s2"},
			StackPin:     func(context.Context, string) (string, error) { return "moved-node", nil },
		}
		ir := testIR("app", st("db", []string{"data"}, ""))
		if err := r.ResolvePlacement(ctx, "app", ir); err != nil {
			t.Fatal(err)
		}
		if ir.Services[0].Placement != "moved-node" {
			t.Fatalf("placement = %q, want moved-node", ir.Services[0].Placement)
		}
	})

	t.Run("Round-robin wins over platform when storage_nodes set", func(t *testing.T) {
		r := &PinResolver{PlatformNode: "plat", StorageNodes: []string{"s1", "s2"}}
		ir := testIR("app", st("db", []string{"data"}, ""))
		if err := r.ResolvePlacement(ctx, "app", ir); err != nil {
			t.Fatal(err)
		}
		got := ir.Services[0].Placement
		if got != "s1" && got != "s2" {
			t.Fatalf("placement = %q, want one of the storage nodes", got)
		}
	})

	t.Run("Platform node is the fallback", func(t *testing.T) {
		r := &PinResolver{PlatformNode: "plat"}
		ir := testIR("app", st("db", []string{"data"}, ""))
		if err := r.ResolvePlacement(ctx, "app", ir); err != nil {
			t.Fatal(err)
		}
		if ir.Services[0].Placement != "plat" {
			t.Fatalf("placement = %q, want plat", ir.Services[0].Placement)
		}
	})

	t.Run("Explicit placement never rewritten", func(t *testing.T) {
		r := &PinResolver{PlatformNode: "plat", StorageNodes: []string{"s1"}}
		ir := testIR("app",
			st("db", []string{"data"}, "explicit-host"),
			st("worker", []string{"jobs"}, "worker"),
			st("api", []string{"cache"}, "manager"))
		if err := r.ResolvePlacement(ctx, "app", ir); err != nil {
			t.Fatal(err)
		}
		if ir.Services[0].Placement != "explicit-host" {
			t.Fatalf("explicit placement clobbered: %q", ir.Services[0].Placement)
		}
		if ir.Services[1].Placement != "worker" || ir.Services[2].Placement != "manager" {
			t.Fatalf("role placements clobbered: %q %q", ir.Services[1].Placement, ir.Services[2].Placement)
		}
	})

	t.Run("Stateless services untouched", func(t *testing.T) {
		r := &PinResolver{PlatformNode: "plat", StorageNodes: []string{"s1"}}
		ir := testIR("app", st("web", nil, ""))
		if err := r.ResolvePlacement(ctx, "app", ir); err != nil {
			t.Fatal(err)
		}
		if ir.Services[0].Placement != "" {
			t.Fatalf("stateless service got placement %q, want \"\"", ir.Services[0].Placement)
		}
	})

	t.Run("No resolver configured leaves IR alone", func(t *testing.T) {
		ir := testIR("app", st("db", []string{"data"}, ""))
		if err := (*PinResolver)(nil).ResolvePlacement(ctx, "app", ir); err != nil {
			t.Fatal(err)
		}
		if ir.Services[0].Placement != "" {
			t.Fatalf("nil resolver mutated placement to %q", ir.Services[0].Placement)
		}
	})

	t.Run("Same stack always resolves to the same node", func(t *testing.T) {
		r := &PinResolver{StorageNodes: []string{"s1", "s2", "s3"}}
		ir1 := testIR("app", st("db", []string{"data"}, ""))
		ir2 := testIR("app", st("db", []string{"data"}, ""))
		if err := r.ResolvePlacement(ctx, "app", ir1); err != nil {
			t.Fatal(err)
		}
		if err := r.ResolvePlacement(ctx, "app", ir2); err != nil {
			t.Fatal(err)
		}
		if ir1.Services[0].Placement != ir2.Services[0].Placement {
			t.Fatalf("unstable assignment: %q vs %q", ir1.Services[0].Placement, ir2.Services[0].Placement)
		}
	})
}

func TestStoragePins(t *testing.T) {
	ctx := context.Background()

	t.Run("Collects distinct pinned nodes", func(t *testing.T) {
		r := &PinResolver{PlatformNode: "plat", StorageNodes: []string{"s1"}}
		ir := testIR("app",
			st("db", []string{"data"}, ""),             // resolves via round-robin → s1
			st("cache", []string{"cache"}, "explicit"), // explicit hostname
			st("web", nil, ""),                         // stateless, skipped
			st("job", []string{"jobs"}, "worker"),      // role-based, skipped
		)
		pins, err := r.StoragePins(ctx, "app", ir)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{"s1": true, "explicit": true}
		if len(pins) != len(want) {
			t.Fatalf("pins = %v, want %d distinct", pins, len(want))
		}
		for _, p := range pins {
			if !want[p] {
				t.Fatalf("unexpected pin %q in %v", p, pins)
			}
		}
	})

	t.Run("Nothing pinned yields empty", func(t *testing.T) {
		r := &PinResolver{PlatformNode: "plat"}
		ir := testIR("app", st("web", nil, ""), st("job", []string{"jobs"}, "worker"))
		pins, err := r.StoragePins(ctx, "app", ir)
		if err != nil {
			t.Fatal(err)
		}
		if len(pins) != 0 {
			t.Fatalf("pins = %v, want empty", pins)
		}
	})

	t.Run("Nil resolver yields empty", func(t *testing.T) {
		ir := testIR("app", st("db", []string{"data"}, ""))
		pins, err := (*PinResolver)(nil).StoragePins(ctx, "app", ir)
		if err != nil {
			t.Fatal(err)
		}
		if len(pins) != 0 {
			t.Fatalf("pins = %v, want empty", pins)
		}
	})
}

// TestSetStorageNodes_LiveRefresh asserts the daemon's settings hook can swap
// the storage-node list at runtime (without a restart) and the next resolution
// pass picks it up — worker nodes added via `join --storage-node` take effect
// immediately.
func TestSetStorageNodes_LiveRefresh(t *testing.T) {
	ctx := context.Background()
	r := &PinResolver{PlatformNode: "plat"}

	// No storage nodes yet → platform fallback.
	ir := testIR("app", st("db", []string{"data"}, ""))
	if err := r.ResolvePlacement(ctx, "app", ir); err != nil {
		t.Fatal(err)
	}
	if got := ir.Services[0].Placement; got != "plat" {
		t.Fatalf("before SetStorageNodes placement = %q, want plat", got)
	}

	// Live refresh: a worker joins as a storage node.
	r.SetStorageNodes([]string{"node-2"})
	ir2 := testIR("app", st("db", []string{"data"}, ""))
	if err := r.ResolvePlacement(ctx, "app", ir2); err != nil {
		t.Fatal(err)
	}
	if got := ir2.Services[0].Placement; got != "node-2" {
		t.Fatalf("after SetStorageNodes placement = %q, want node-2", got)
	}

	// Empty list reverts to the platform fallback.
	r.SetStorageNodes(nil)
	ir3 := testIR("app", st("db", []string{"data"}, ""))
	if err := r.ResolvePlacement(ctx, "app", ir3); err != nil {
		t.Fatal(err)
	}
	if got := ir3.Services[0].Placement; got != "plat" {
		t.Fatalf("after SetStorageNodes(nil) placement = %q, want plat", got)
	}

	// Nil receiver is safe.
	(*PinResolver)(nil).SetStorageNodes([]string{"x"})
}
