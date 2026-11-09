package cluster

import (
	"fmt"
	"sort"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// IsStorageNode reports whether a node is a storage node.
//
// The VALUE is compared, never the key's presence. A `pmcluster.storage` key
// holding "" is not "no label": a presence test reads it as a storage node, and
// an old writer (or a hand-run `docker node update --label-add ...=`) can leave
// exactly that on a worker. Empty values are removed by SetNodeLabel, so the
// only correct read of a label this platform owns is value equality.
func IsStorageNode(n runtime.Node) bool {
	return n.Labels[runtime.StorageNodeLabel] == "true"
}

// EmptyNodeLabels returns "hostname (key)" for every pmcluster.* node label that
// carries an empty value, sorted for a stable message.
//
// This is the consistency question `pmcluster doctor` asks: an empty-valued
// label is invisible in `docker node inspect` at a glance but still present to
// anything that matches on the key, which is how a worker came to look like a
// storage node.
func EmptyNodeLabels(nodes []runtime.Node) []string {
	var out []string
	for _, n := range nodes {
		for key, value := range n.Labels {
			if value != "" || !hasPmclusterPrefix(key) {
				continue
			}
			host := n.Hostname
			if host == "" {
				host = n.ID
			}
			out = append(out, fmt.Sprintf("%s (%s)", host, key))
		}
	}
	sort.Strings(out)
	return out
}

// hasPmclusterPrefix reports whether key is in the platform's node-label
// namespace. Both spellings are checked: the node labels this platform manages
// are `pmcluster.storage`, `pmcluster.leader` and `pmcluster.version`, while the
// service labels use the `io.pmcluster.` prefix.
func hasPmclusterPrefix(key string) bool {
	const (
		nodeNS = "pmcluster."
		ioNS   = "io.pmcluster."
	)
	return len(key) > len(nodeNS) && (key[:len(nodeNS)] == nodeNS || key[:len(ioNS)] == ioNS)
}
