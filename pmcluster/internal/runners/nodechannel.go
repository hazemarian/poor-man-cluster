package runners

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxNodeResponse bounds one answer on the node-facing channel. The assignment is
// small; the cluster snapshot is bigger (every setting, every secret's ciphertext and
// every API token, tens of rows) but still well under this, and the cap is what stops
// a corrupt or hostile leader from exhausting a follower's memory.
const maxNodeResponse = 8 << 20

// Do performs one request on the node-facing channel: the same certificate, the same
// CA and swarm checks, and the same candidate walk the assignment fetch uses. It
// exists so the follower's snapshot mirror does not grow a second copy of that
// transport — the copy that would drift is the one nobody watches.
//
// Semantics the caller depends on: the FIRST candidate that yields an HTTP response
// wins, whatever its status. A 403 is the leader's answer about this node's
// certificate; walking to the next manager would turn one clear refusal into a timeout.
// Only a transport failure (connect, TLS, timeout) moves on.
func (c *Client) Do(ctx context.Context, method, path string, header map[string]string) (int, http.Header, []byte, string, error) {
	client, bases, err := c.transport(ctx)
	if err != nil {
		return 0, nil, nil, "", err
	}
	var errs []string
	for _, base := range bases {
		req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
		if err != nil {
			return 0, nil, nil, "", err
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", base, err))
			continue
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxNodeResponse))
		_ = resp.Body.Close()
		if rerr != nil {
			errs = append(errs, fmt.Sprintf("%s: read body: %v", base, rerr))
			continue
		}
		return resp.StatusCode, resp.Header, body, base, nil
	}
	if len(errs) == 0 {
		return 0, nil, nil, "", fmt.Errorf("no candidate address for the leader")
	}
	return 0, nil, nil, "", fmt.Errorf("%s %s failed on every candidate (%s)", method, path, strings.Join(errs, "; "))
}
