package cluster

import (
	"reflect"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// A label present with an empty value is not "no label", and it is not a
// storage node: the read compares the value. `nxt-sw-3-w` carried
// `pmcluster.storage: ""` and a presence test read it as storage.
func TestIsStorageNodeComparesTheValue(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"true", map[string]string{runtime.StorageNodeLabel: "true"}, true},
		{"empty value", map[string]string{runtime.StorageNodeLabel: ""}, false},
		{"false", map[string]string{runtime.StorageNodeLabel: "false"}, false},
		{"another label only", map[string]string{runtime.LeaderNodeLabel: "true"}, false},
		{"no labels", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsStorageNode(runtime.Node{Hostname: "n", Labels: tc.labels}); got != tc.want {
				t.Fatalf("IsStorageNode = %v, want %v (labels %v)", got, tc.want, tc.labels)
			}
		})
	}
}

// The consistency check names every pmcluster.* label that carries an empty
// value, on any node, sorted so the message is stable.
func TestEmptyNodeLabelsReportsEmptyValuedPmclusterLabels(t *testing.T) {
	nodes := []runtime.Node{
		{Hostname: "nxt-sw-3-w", Labels: map[string]string{
			runtime.StorageNodeLabel: "",
			"unrelated.example":      "",
		}},
		{Hostname: "nxt-sw-4-m", Labels: map[string]string{
			runtime.LeaderNodeLabel:  "",
			runtime.StorageNodeLabel: "true",
		}},
		{Hostname: "nxt-sw-5-m", Labels: map[string]string{
			runtime.StorageNodeLabel: "true",
			"some.other.label":       "",
		}},
	}
	want := []string{
		"nxt-sw-3-w (pmcluster.storage)",
		"nxt-sw-4-m (pmcluster.leader)",
	}
	if got := EmptyNodeLabels(nodes); !reflect.DeepEqual(got, want) {
		t.Fatalf("EmptyNodeLabels = %v, want %v", got, want)
	}
}

// A clean swarm reports nothing — the check must not fire on healthy labels.
func TestEmptyNodeLabelsCleanSwarm(t *testing.T) {
	nodes := []runtime.Node{{Hostname: "a", Labels: map[string]string{runtime.StorageNodeLabel: "true"}}}
	if got := EmptyNodeLabels(nodes); len(got) != 0 {
		t.Fatalf("EmptyNodeLabels = %v, want none", got)
	}
}
