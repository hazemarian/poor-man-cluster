package engine

import "testing"

// TestPlacementFor pins the platform's placement rules. They were extracted
// verbatim from the compose writer, so this table is the contract: if the
// behaviour moves, this test is what makes the move visible instead of silent.
func TestPlacementFor(t *testing.T) {
	cases := []struct {
		name             string
		declared         string
		pinNode          string
		stateful, global bool
		wantRole         Role
		wantNode         string
		wantPinned       bool
	}{
		{
			name:     "manager restricts by role",
			declared: DeclaredManager,
			wantRole: RoleManager,
		},
		{
			name:     "worker restricts by role",
			declared: DeclaredWorker,
			wantRole: RoleWorker,
		},
		{
			name:     "unconstrained stateless service is left free",
			declared: "",
			pinNode:  "nxt-sw-1-m",
			stateful: false,
		},
		{
			name:       "stateful service auto-pins to the platform pin node",
			declared:   "",
			pinNode:    "nxt-sw-1-m",
			stateful:   true,
			wantNode:   "nxt-sw-1-m",
			wantPinned: true,
		},
		{
			// Global services run on every node; pinning one would make it
			// unschedulable on the node that is not the pin.
			name:     "global service never auto-pins",
			declared: "",
			pinNode:  "nxt-sw-1-m",
			stateful: true,
			global:   true,
		},
		{
			name:       "stateful service with no pin node stays free",
			declared:   "",
			stateful:   true,
			wantPinned: false,
		},
		{
			name:       "explicit hostname is a pin",
			declared:   "nxt-sw-5-m",
			wantNode:   "nxt-sw-5-m",
			wantPinned: true,
		},
		{
			// An explicit pin wins over the platform's pin node: the operator
			// said where it goes.
			name:       "explicit pin beats the platform pin node",
			declared:   "nxt-sw-4-m",
			pinNode:    "nxt-sw-1-m",
			stateful:   true,
			wantNode:   "nxt-sw-4-m",
			wantPinned: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PlacementFor(tc.declared, tc.pinNode, tc.stateful, tc.global)
			if got.Role != tc.wantRole {
				t.Errorf("role = %q, want %q", got.Role, tc.wantRole)
			}
			if got.Node != tc.wantNode {
				t.Errorf("node = %q, want %q", got.Node, tc.wantNode)
			}
			if got.Pinned() != tc.wantPinned {
				t.Errorf("pinned = %v, want %v", got.Pinned(), tc.wantPinned)
			}
			// A placement must never carry both: engines combine constraints
			// conjunctively, so a pin plus a role can be unsatisfiable.
			if got.Node != "" && got.Role != RoleAny {
				t.Errorf("placement carries both a node pin and a role: %+v", got)
			}
		})
	}
}
