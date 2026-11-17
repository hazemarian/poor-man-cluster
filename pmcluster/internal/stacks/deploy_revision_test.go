package stacks

import (
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// TestReusableFailedRevision is the rule that stops a failing stack from minting a
// revision per reconcile pass.
//
// Measured on production: a crash-looping service produced 8 consecutive revisions
// with ONE distinct rendered manifest, each carrying the same error, because the
// rendered hash is only stamped after a successful apply — so every pass looked like
// drift. The console then showed a wall of "deploy failed".
func TestReusableFailedRevision(t *testing.T) {
	failed := &store.StackRevision{
		Revision:     100,
		SourceYAML:   "app: demo\nservices: {}\n",
		RenderedYAML: "services:\n  web: {image: x}\n",
		RenderedHash: "", // never stamped: the apply did not succeed
	}
	applied := &store.StackRevision{
		Revision:     101,
		SourceYAML:   failed.SourceYAML,
		RenderedYAML: failed.RenderedYAML,
		RenderedHash: "d32e59df49d9", // a successful apply
	}

	tests := []struct {
		name     string
		cur      *store.StackRevision
		rendered string
		source   string
		want     bool
	}{
		{"failed revision, identical deploy: retry in place", failed, failed.RenderedYAML, failed.SourceYAML, true},
		{"already applied: a new deploy is a new revision", applied, failed.RenderedYAML, failed.SourceYAML, false},
		{"the render changed: this is a different deploy", failed, "services:\n  web: {image: y}\n", failed.SourceYAML, false},
		{"the source changed: keep the new intent on its own revision", failed, failed.RenderedYAML, "app: demo\nservices: {x: y}\n", false},
		{"no current revision", nil, failed.RenderedYAML, failed.SourceYAML, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := reusableFailedRevision(tc.cur, tc.rendered, tc.source); got != tc.want {
				t.Fatalf("reusableFailedRevision = %v, want %v", got, tc.want)
			}
		})
	}
}
