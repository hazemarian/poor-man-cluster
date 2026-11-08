package telemetry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

// testClock is a hand-cranked clock: an outage of any length costs the test
// microseconds and no sleeps.
type testClock struct{ t time.Time }

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time          { return c.t }
func (c *testClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// collect builds a reporter whose events land in the returned slice.
func collect(t *testing.T, interval time.Duration, clock *testClock, probe func() bool) (*failureReporter, *[]FailureEvent) {
	t.Helper()
	events := &[]FailureEvent{}
	r := newFailureReporter(interval, clock.Now, probe, func(ev FailureEvent) {
		*events = append(*events, ev)
	})
	return r, events
}

// TestFailureReporter_ManyFailuresInsideIntervalProduceOneLine is the property
// the journal depends on: a broken endpoint costs O(1) lines per interval, not
// one per failed export. 500 failures inside the first interval must produce
// exactly one line.
func TestFailureReporter_ManyFailuresInsideIntervalProduceOneLine(t *testing.T) {
	clock := newTestClock()
	r, events := collect(t, 5*time.Minute, clock, func() bool { return false })

	err := errors.New("dial tcp 10.7.224.15:4318: connect: connection refused")
	for i := 0; i < 500; i++ {
		r.Handle(err)
		if i%50 == 0 {
			clock.Advance(10 * time.Second) // still well inside 5 minutes
		}
	}

	if got := len(*events); got != 1 {
		t.Fatalf("500 failures inside one interval produced %d lines, want 1", got)
	}
	ev := (*events)[0]
	if ev.Kind != FailureFirst {
		t.Errorf("kind = %v, want FailureFirst", ev.Kind)
	}
	if ev.Err != err {
		t.Errorf("first line lost the error: %v", ev.Err)
	}
	if ev.Suppressed != 0 {
		t.Errorf("first line suppressed = %d, want 0 (nothing suppressed yet)", ev.Suppressed)
	}
}

// TestFailureReporter_SummaryCarriesTheSuppressedCount: when the interval
// elapses the reporter emits ONE line whose count is every failure it swallowed.
func TestFailureReporter_SummaryCarriesTheSuppressedCount(t *testing.T) {
	clock := newTestClock()
	r, events := collect(t, 5*time.Minute, clock, func() bool { return false })
	err := errors.New("connection refused")

	r.Handle(err) // first line
	clock.Advance(4 * time.Minute)
	for i := 0; i < 250; i++ {
		r.Handle(err)
	}
	if len(*events) != 1 {
		t.Fatalf("lines inside the interval = %d, want 1", len(*events))
	}

	clock.Advance(time.Minute + time.Second) // past the interval
	r.Handle(err)

	if len(*events) != 2 {
		t.Fatalf("lines after the interval = %d, want 2", len(*events))
	}
	ev := (*events)[1]
	if ev.Kind != FailureOngoing {
		t.Errorf("kind = %v, want FailureOngoing", ev.Kind)
	}
	if ev.Suppressed != 251 {
		t.Errorf("suppressed = %d, want 251 (the 250 swallowed + the one that triggered the summary)", ev.Suppressed)
	}
}

// TestFailureReporter_RecoversOnce: a recovery is reported exactly once, and the
// next failure starts a fresh cycle (first line again) rather than resuming the
// old count.
func TestFailureReporter_RecoversOnce(t *testing.T) {
	clock := newTestClock()
	up := false
	r, events := collect(t, 5*time.Minute, clock, func() bool { return up })
	err := errors.New("connection refused")

	r.Handle(err) // first failure
	clock.Advance(time.Minute)
	r.Handle(err) // suppressed
	clock.Advance(4 * time.Minute)
	up = true
	r.Handle(err) // interval due, probe says the collector answers

	if len(*events) != 2 {
		t.Fatalf("lines = %d, want 2 (first failure + recovery)", len(*events))
	}
	rec := (*events)[1]
	if rec.Kind != FailureRecovered {
		t.Fatalf("kind = %v, want FailureRecovered", rec.Kind)
	}
	if rec.Suppressed != 2 {
		t.Errorf("recovery suppressed = %d, want 2", rec.Suppressed)
	}

	// No further lines until something fails again.
	clock.Advance(time.Hour)
	r.Handle(nil) // a nil error is still "a failure signal" from the SDK's view
	clock.Advance(time.Hour)
	if len(*events) != 3 {
		t.Fatalf("lines = %d after a long quiet period, want 3 (no repeats)", len(*events))
	}
	if (*events)[2].Kind != FailureFirst {
		t.Errorf("post-recovery kind = %v, want FailureFirst (a new cycle)", (*events)[2].Kind)
	}
}

// TestFailureReporter_LongOutageIsLogBounded drives an hour of continuous
// failure — one export failure every 100ms — through the reporter and asserts the
// total number of lines, which is what the journal actually pays.
func TestFailureReporter_LongOutageIsLogBounded(t *testing.T) {
	clock := newTestClock()
	r, events := collect(t, 5*time.Minute, clock, func() bool { return false })
	err := errors.New("connection refused")

	const total = 55 * time.Minute
	const step = 100 * time.Millisecond
	failures := 0
	for elapsed := time.Duration(0); elapsed < total; elapsed += step {
		r.Handle(err)
		failures++
		clock.Advance(step)
	}

	// 1 first line + one summary at each complete 5-minute boundary. The last
	// partial interval never ticks: a tick needs an export AFTER it elapses.
	want := 1 + int((total-time.Nanosecond)/(5*time.Minute))
	if len(*events) != want {
		t.Fatalf("%d failures over %s produced %d lines, want %d", failures, total, len(*events), want)
	}
	if failures < 30000 {
		t.Fatalf("test did not actually simulate a storm: %d failures", failures)
	}
	// Every summary must carry a count; that is the whole point of the line.
	for _, ev := range (*events)[1:] {
		if ev.Suppressed == 0 {
			t.Errorf("summary line with no suppressed count: %+v", ev)
		}
	}
	t.Logf("%d export failures over %s → %d log lines", failures, total, len(*events))
}

// TestInit_RateLimitsSDKErrors proves the wiring, not just the type: Init must
// replace the SDK's default error handler, so a burst of SDK-level failures
// produces one line.
func TestInit_RateLimitsSDKErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var lines []FailureEvent
	shutdown, err := Init(context.Background(), Options{
		Endpoint: srv.URL,
		Failures: func(ev FailureEvent) { lines = append(lines, ev) },
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	handler := otel.GetErrorHandler()
	for i := 0; i < 20; i++ {
		handler.Handle(errors.New("export failed"))
	}
	if len(lines) != 1 {
		t.Errorf("20 SDK failures produced %d lines, want 1", len(lines))
	}
}
