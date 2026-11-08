package telemetry

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// DefaultFailureLogInterval is how long the exporter's failure path stays quiet
// between lines. The OTel SDK calls the global error handler for EVERY failed
// export, and its default handler writes a line each time; a provider with a 10s
// metric interval, a 5s batch timer and 15 log attributes turns one closed port
// into thousands of journal lines per minute. Five minutes keeps an outage
// visible in the journal (one line, then a summary) without letting it compete
// with the container runtime for disk writes.
const DefaultFailureLogInterval = 5 * time.Minute

// FailureKind says what one FailureEvent reports. The three kinds together mean
// a broken endpoint costs O(1) lines per interval and a working one costs two
// lines per outage.
type FailureKind int

const (
	// FailureFirst is the first failure after a healthy period. It carries the
	// error text (and, on the next line, only counts).
	FailureFirst FailureKind = iota
	// FailureOngoing is the periodic summary for failures the reporter has
	// swallowed since the previous line.
	FailureOngoing
	// FailureRecovered is emitted once when the endpoint answers again, and
	// carries how many failures went unreported.
	FailureRecovered
)

// FailureEvent is one line the caller should log. Callers format it; the
// reporter decides when it happens, which is what keeps the volume bounded.
type FailureEvent struct {
	Kind       FailureKind
	Err        error
	Suppressed int
	Interval   time.Duration
}

// failureReporter turns an unbounded stream of export errors into one line per
// interval plus one recovery line.
//
// It is deliberately indifferent to what the error says: the only question it
// asks is whether the endpoint answers a dial again, because a collector that
// is up but rejecting payloads still produces a failure per export and must be
// summarised, not narrated.
type failureReporter struct {
	interval time.Duration
	now      func() time.Time
	probe    func() bool
	report   func(FailureEvent)

	mu         sync.Mutex
	failing    bool
	lastEmit   time.Time
	suppressed int
}

// newFailureReporter wires a reporter. now/probe/report are injectable so tests
// can drive an outage of any length in microseconds.
func newFailureReporter(interval time.Duration, now func() time.Time, probe func() bool, report func(FailureEvent)) *failureReporter {
	if interval <= 0 {
		interval = DefaultFailureLogInterval
	}
	if now == nil {
		now = time.Now
	}
	if probe == nil {
		probe = func() bool { return false }
	}
	if report == nil {
		report = func(ev FailureEvent) { log.Printf("%s", formatFailure(ev)) }
	}
	return &failureReporter{interval: interval, now: now, probe: probe, report: report}
}

// Handle is an otel.ErrorHandlerFunc. It never blocks on the network except
// when a summary is due, and then only for one dial.
func (r *failureReporter) Handle(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	if !r.failing {
		r.failing = true
		r.lastEmit = now
		r.suppressed = 0
		r.report(FailureEvent{Kind: FailureFirst, Err: err, Interval: r.interval})
		return
	}

	r.suppressed++
	if now.Sub(r.lastEmit) < r.interval {
		return
	}

	// A summary is due. Ask the endpoint itself whether it is back: the SDK
	// only ever reports failures, so a recovery is otherwise indistinguishable
	// from a quiet outage that keeps failing.
	suppressed := r.suppressed
	r.suppressed = 0
	r.lastEmit = now
	if r.probe() {
		r.failing = false
		r.report(FailureEvent{Kind: FailureRecovered, Err: err, Suppressed: suppressed, Interval: r.interval})
		return
	}
	r.report(FailureEvent{Kind: FailureOngoing, Err: err, Suppressed: suppressed, Interval: r.interval})
}

// formatFailure renders the default (stdlib-log) wording. The daemon supplies
// its own reporter that goes through zerolog; this exists so a one-shot CLI
// command is covered too, without pulling a logger through the SDK.
func formatFailure(ev FailureEvent) string {
	switch ev.Kind {
	case FailureRecovered:
		return fmt.Sprintf("OTel export recovered after %d suppressed failure(s) in %s", ev.Suppressed, ev.Interval)
	case FailureOngoing:
		return fmt.Sprintf("OTel export still failing: %d export(s) failed in the last %s: %v", ev.Suppressed, ev.Interval, ev.Err)
	default:
		return fmt.Sprintf("OTel export failed; repeats suppressed for %s: %v", ev.Interval, ev.Err)
	}
}

// probeEndpoint dials addr once and reports whether it answered. One dial per
// interval, never per export.
func probeEndpoint(addr string) func() bool {
	return func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		d := net.Dialer{}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return false
		}
		return conn.Close() == nil
	}
}
