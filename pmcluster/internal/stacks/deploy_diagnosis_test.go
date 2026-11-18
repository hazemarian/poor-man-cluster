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

	blocked, why := svc.levelNotReady(context.Background(), "demo", []string{"db", "web"})
	if why == "" {
		t.Fatal("no diagnosis produced for an unready level")
	}
	if len(blocked) != 1 || blocked[0] != "web" {
		t.Fatalf("blocked = %v, want [web]: the message must name the service that is not healthy, not the level", blocked)
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
	if !strings.Contains(why, "task failed:") {
		t.Errorf("the diagnosis does not say the task failed: %q", why)
	}
	// The NEWEST task must be quoted, not the first one that ever ran: a crash-looping
	// service's oldest failure is history, its newest is the evidence.
	if strings.Contains(why, "non-zero exit") {
		t.Errorf("diagnosis quoted the oldest task instead of the newest: %q", why)
	}
}

// TestLevelNotReady_PrefersTheFailedTaskOverAStartingOne — at the instant a wait expires, a
// crash-looping service usually has a brand-new task in "starting". Quoting that one (the old
// behaviour: "newest task starting (\"starting\")") hides the failure that keeps causing it.
func TestLevelNotReady_PrefersTheFailedTaskOverAStartingOne(t *testing.T) {
	f := &waitFake{
		svcs: []runtime.Service{{ID: "web-id", Name: "demo_web", Desired: 1, Replicas: 0}},
		tasks: map[string][]runtime.ServiceTask{
			"web-id": {
				{TaskID: "failed", State: "failed", Error: "registering the Telegram webhook: 404", StartedAt: 100, FinishedAt: 160},
				{TaskID: "starting", State: "starting", StartedAt: 200},
			},
		},
	}
	svc := waitService(t, f)
	_, why := svc.levelNotReady(context.Background(), "demo", []string{"web"})
	if !strings.Contains(why, "404") {
		t.Errorf("the diagnosis hid the failure behind a fresh task: %q", why)
	}
	if strings.Contains(why, "starting") {
		t.Errorf("the diagnosis quoted the transient task: %q", why)
	}
}

// TestLevelNotReady_DoesNotCallATransitionTaskAFailure — Docker fills a task's Error with progress
// words too ("prepared", "starting"). Selecting on Error produced "last failed task ready
// (\"prepared\")" in production: a sentence that contradicts itself. Only failed/rejected states count.
func TestLevelNotReady_DoesNotCallATransitionTaskAFailure(t *testing.T) {
	f := &waitFake{
		svcs: []runtime.Service{{ID: "web-id", Name: "demo_web", Desired: 1, Replicas: 0}},
		tasks: map[string][]runtime.ServiceTask{
			"web-id": {
				{TaskID: "ready", State: "ready", Error: "prepared", StartedAt: 200},
			},
		},
	}
	svc := waitService(t, f)
	_, why := svc.levelNotReady(context.Background(), "demo", []string{"web"})
	if strings.Contains(why, "task ready:") {
		t.Errorf("a ready task was reported as a failure: %q", why)
	}
	if !strings.Contains(why, "newest task ready") {
		t.Errorf("the diagnosis does not describe the state it found: %q", why)
	}
}

// TestLevelNotReady_QuietWhenEverythingIsReady — no false alarms in the message of a healthy deploy.
func TestLevelNotReady_QuietWhenEverythingIsReady(t *testing.T) {
	f := &waitFake{svcs: []runtime.Service{{ID: "db-id", Name: "demo_db", Desired: 1, Replicas: 1}}}
	svc := waitService(t, f)
	if blocked, why := svc.levelNotReady(context.Background(), "demo", []string{"db"}); why != "" || len(blocked) != 0 {
		t.Errorf("diagnosis for a ready level = %q / %v, want empty", why, blocked)
	}
}
