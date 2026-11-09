package remote

import (
	"context"
	"net/http"
	"net/url"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
)

// MoveLeader asks the daemon to force Swarm leadership onto node and reports
// where leadership ended up. The daemon runs the same election sequence the
// local CLI would; the response is the outcome, not a promise.
func (c *Client) MoveLeader(ctx context.Context, node string) (cluster.LeaderMove, error) {
	var out struct {
		Leader    string `json:"leader"`
		Previous  string `json:"previous"`
		Elections int    `json:"elections"`
	}
	if err := c.do(ctx, http.MethodPost, "/cluster/leader/"+url.PathEscape(node), nil, &out); err != nil {
		return cluster.LeaderMove{}, err
	}
	return cluster.LeaderMove{From: out.Previous, Leader: out.Leader, Elections: out.Elections}, nil
}
