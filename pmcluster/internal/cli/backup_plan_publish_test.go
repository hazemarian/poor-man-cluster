package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// failingDocker is a runtime.Client that only has to fail ServiceList.
type failingDocker struct {
	runtime.Client // nil embed: any other call panics, which is what we want
	err            error
}

func (f failingDocker) ServiceList(context.Context) ([]runtime.Service, error) { return nil, f.err }

// A worker may not read the swarm, so publishing there fails forever. It must say
// so once, not ninety-six times a day: a warning that repeats is a warning nobody
// reads.
func TestPlanPublishLogsAFailureOncePerDistinctError(t *testing.T) {
	prev := planPublishInterval
	planPublishInterval = 5 * time.Millisecond
	t.Cleanup(func() { planPublishInterval = prev })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	dc := failingDocker{err: errors.New("This node is not a swarm manager")}
	var lines []string
	publishBackupPlan(ctx, dc, func(format string, args ...any) {
		lines = append(lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
	})
	<-ctx.Done()

	if len(lines) != 1 {
		t.Fatalf("logged %d times, want exactly 1 for a permanently failing worker:\n%s",
			len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], "not a swarm manager") {
		t.Errorf("the one line should say why: %q", lines[0])
	}
}
