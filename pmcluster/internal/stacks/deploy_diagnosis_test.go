package stacks

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// TestHealthWaitTimeout — a manifest may ask for more patience than the platform default, and a typo
// must not fail a deploy that would otherwise work.
func TestHealthWaitTimeout(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"", deployWaitTimeout},
		{"90s", 90 * time.Second},
		{"10m", 10 * time.Minute},
		{"nonsense", deployWaitTimeout},
		{"0s", deployWaitTimeout},
		{"-5m", deployWaitTimeout},
	} {
		if got := healthWaitTimeout(tc.raw); got != tc.want {
			t.Errorf("healthWaitTimeout(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// TestLevelNotReady_NamesTheServiceAndItsTaskError is the whole point of item one: "timed out waiting
// for level [db, web]" cost hours on this platform, while the answer — a web task exiting 1 with a
// Telegram 404 — was in the task's own error the whole time.
func TestLevelNotReady_NamesTheServiceAndItsTaskError(t *testing.T) {
	f := &waitFake{
		svcs: []runtime.Service{
			{ID: "db-id", Name: "demo_db", Desired: 1, Replicas: 1},
			{ID: "web-id", Name: "demo_web", Desired: 1, Replicas: 0},
		},
		tasks: map[string][]runtime.ServiceTask{
			"web-id": {
				{TaskID: "old", State: "failed", Error: "task: non-zero exit (1)", StartedAt: 100},
				{TaskID: "new", State: "failed", Error: "registering the Telegram webhook: 404", StartedAt: 300},
			},
		},
	}
	svc := waitService(t, f)

	why := svc.levelNotReady(context.Background(), "demo", []string{"db", "web"})
	if why == "" {
		t.Fatal("no diagnosis produced for an unready level")
	}
	if !strings.Contains(why, "web: 0/1 running") {
		t.Errorf("diagnosis does not give the replica count: %q", why)
	}
	if !strings.Contains(why, "404") {
		t.Errorf("diagnosis does not carry the task's own error: %q", why)
	}
	if strings.Contains(why, "db") {
		t.Errorf("a ready service was reported as the problem: %q", why)
	}
	// The NEWEST task must be quoted, not the first one that ever ran: a crash-looping
	// service's oldest failure is history, its newest is the evidence.
	if strings.Contains(why, "non-zero exit") {
		t.Errorf("diagnosis quoted the oldest task instead of the newest: %q", why)
	}
}

// TestLevelNotReady_QuietWhenEverythingIsReady — no false alarms in the message of a healthy deploy.
func TestLevelNotReady_QuietWhenEverythingIsReady(t *testing.T) {
	f := &waitFake{svcs: []runtime.Service{{ID: "db-id", Name: "demo_db", Desired: 1, Replicas: 1}}}
	svc := waitService(t, f)
	if why := svc.levelNotReady(context.Background(), "demo", []string{"db"}); why != "" {
		t.Errorf("diagnosis for a ready level = %q, want empty", why)
	}
}
