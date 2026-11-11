package runners

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// Refusing to run a runner that cannot fit the node.
//
// A container memory cap larger than the machine is not a cap. On 2026-11-10
// nxt-sw-3-w (1839 MB of RAM) was allowed `github_runner_memory=2560m` — 139 % of
// the host — and the first CI build that used it OOM-killed the node. sshd died
// with it, so the node could not be restarted remotely and the cluster ran two of
// three managers for hours. Docker will happily accept a `--memory` larger than
// the host: the limit is enforced per cgroup, and the cgroup's "limit" then
// exceeds the resource, which is permission rather than a boundary.
//
// The rule is checked where the assignment is RESOLVED (the leader) and again
// wherever a node reports its own state, so it is re-evaluated on every pass and
// cannot be outlived by a settings change.

// MemoryCeilingPercent is the share of a node's total RAM a runner's memory cap
// may claim.
//
// Why 70 % and not 60 %: the platform's own default runner memory is 2560m
// (the value that completed a govulncheck run), and 60 % of a 3852 MB node is
// 2311 MB — a 60 % rule would refuse the documented default on a healthy 4 GB
// worker, which is not a boundary but an outage. 70 % of 3852 MB is 2696 MB, so
// the default fits there and 2560m on 1839 MB (139 %) is refused, which is the
// pair of cases this rule exists for. It is also the same ceiling the control
// plane's own cgroup is given (MemoryMax, internal/cli/daemon.go): the workload
// may claim the same share the control plane may, and no more.
const MemoryCeilingPercent = 70

// ErrRunnerMemory marks a refusal that is about arithmetic, not about a missing
// setting. Callers that need to name the two numbers type-assert MemoryRefusal.
var ErrRunnerMemory = errors.New("runner memory cap is larger than this node can survive")

// MemoryRefusal names both numbers. "The cap does not fit" without the host's
// size is the error message an operator cannot act on: 2560m is absurd on 1839 MB
// and ordinary on 16 GB, and only the pair says which node needs the fix.
type MemoryRefusal struct {
	Node    string
	HostMiB int64
	CapMiB  int64
	// Cap is the configured value as written, so the message quotes setting.
	Cap string
}

func (e *MemoryRefusal) Error() string {
	return fmt.Sprintf("%s has %d MB; a %s runner can OOM this node and take it down — lower github_runner_memory or pick a bigger node",
		e.Node, e.HostMiB, e.Cap)
}

func (e *MemoryRefusal) Unwrap() error { return ErrRunnerMemory }

// ParseMemoryMiB reads a Docker memory quantity ("2560m", "3g", "1.5g", "512b")
// as MiB, using Docker's binary multipliers.
//
// Docker's suffixes are 1024-based (m = MiB), which is why the comparison against
// a node's MemTotal — reported in MiB by nodestat — is apples to apples. A bare
// number is bytes, as Docker reads it.
func ParseMemoryMiB(q string) (int64, error) {
	s := strings.ToLower(strings.TrimSpace(q))
	if s == "" {
		return 0, fmt.Errorf("empty memory quantity")
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'b':
		s, mult = s[:len(s)-1], 1
	case 'k':
		s, mult = s[:len(s)-1], 1024
	case 'm':
		s, mult = s[:len(s)-1], 1024*1024
	case 'g':
		s, mult = s[:len(s)-1], 1024*1024*1024
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("not a docker memory quantity: %q", q)
	}
	return int64(v*float64(mult)) / (1024 * 1024), nil
}

// CheckMemoryFit refuses a cap that claims more than MemoryCeilingPercent of a
// node's RAM.
//
// Unknown RAM is NOT a refusal: a node whose telemetry has not arrived yet (or a
// cluster that never enabled it) must still be able to run CI, and refusing on
// missing information would turn a monitoring gap into an outage. The caller logs
// that once per node instead — see AssignmentService.NodeMemory.
func CheckMemoryFit(node string, hostMiB int64, known bool, cap string) error {
	if !known || hostMiB <= 0 {
		return nil
	}
	capMiB, err := ParseMemoryMiB(cap)
	if err != nil || capMiB <= 0 {
		// An unparseable quantity is ValidateMemory's refusal, not this one.
		return nil
	}
	// Integer arithmetic on purpose: no float rounding at the boundary, so a cap
	// exactly at the share is allowed and one MiB over is not.
	if capMiB*100 > hostMiB*MemoryCeilingPercent {
		return &MemoryRefusal{Node: node, HostMiB: hostMiB, CapMiB: capMiB, Cap: strings.TrimSpace(cap)}
	}
	return nil
}

// NodeMemoryFreshFor bounds how old a node-stats sample may be before its RAM
// counts as unknown. RAM rarely changes, but a rebuilt node can have different
// RAM, and a sample that old means the node is not reporting at all — which is
// itself worth not trusting.
const NodeMemoryFreshFor = time.Hour

// NodeMemoryMiB returns a node's total RAM from the leader's node-stats rows,
// which is the only place a worker's RAM exists (the swarm node list carries no
// memory). known=false means "no recent sample": the caller allows the runner and
// says so once.
func NodeMemoryMiB(ctx context.Context, st *store.Store, node string, now time.Time) (int64, bool) {
	if st == nil || strings.TrimSpace(node) == "" {
		return 0, false
	}
	rows, err := st.ListNodeStats(ctx)
	if err != nil {
		return 0, false
	}
	for _, row := range rows {
		if !strings.EqualFold(row.Hostname, node) {
			continue
		}
		if row.MemTotalMB <= 0 {
			return 0, false
		}
		if row.SampleTS > 0 && now.Sub(time.Unix(row.SampleTS, 0)) > NodeMemoryFreshFor {
			return 0, false
		}
		return row.MemTotalMB, true
	}
	return 0, false
}
