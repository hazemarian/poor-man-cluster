package manifest

import (
	"strings"
	"testing"
)

// TestTranslate_PlacementPreferences pins the soft-placement surface added for
// the leader-pinned collector: a preference renders as a Compose `spread`
// expression, sits ALONGSIDE the role constraint (never replacing it), and a
// malformed expression is rejected instead of silently rendering nothing.
func TestTranslate_PlacementPreferences(t *testing.T) {
	const pref = "node.labels.pmcluster.leader==true"

	app := baseApp()
	app.Services["api"].Placement = "manager"
	app.Services["api"].Preferences = []string{pref}
	s, cf := renderCompose(t, app, nil)

	p := deployOf(t, cf, "api").Placement
	if p == nil {
		t.Fatal("placement block missing")
	}
	if len(p.Constraints) != 1 || p.Constraints[0] != "node.role == manager" {
		t.Fatalf("constraints = %+v, want the manager floor", p.Constraints)
	}
	if len(p.Preferences) != 1 || p.Preferences[0].Spread != pref {
		t.Fatalf("preferences = %+v, want one spread %q", p.Preferences, pref)
	}
	if !strings.Contains(s, "spread: "+pref) {
		t.Errorf("preference did not render as a spread expression:\n%s", s)
	}

	// A preference must never be turned into a constraint: that is what would
	// strand the task when no node carries the label.
	for _, c := range p.Constraints {
		if strings.Contains(c, "pmcluster.leader") {
			t.Fatalf("preference leaked into constraints: %q", c)
		}
	}

	// Validation: empty, non-node and multi-key expressions are refused.
	for _, bad := range []string{"", "   ", "labels.x==true", "node.labels.a==1,node.labels.b==2", "node.labels.a == 1"} {
		app := baseApp()
		app.Services["api"].Preferences = []string{bad}
		mustFail(t, app, "preferences[0]")
	}
}
