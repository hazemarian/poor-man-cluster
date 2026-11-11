package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// watchdogHarness is one node's worth of watchdog state: a fake clock, a fake
// systemd, and a probe whose answer the test flips. Every decision in
// watchOnce is reachable through it, with no sleeps and no systemd.
type watchdogHarness struct {
	dir        string
	now        time.Time
	failing    bool
	active     bool
	age        time.Duration
	ageKnown   bool
	restarts   int
	restartErr error
	out        bytes.Buffer
}

func newWatchdogHarness(t *testing.T) *watchdogHarness {
	t.Helper()
	h := &watchdogHarness{
		dir:      t.TempDir(),
		now:      time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC),
		failing:  true,
		active:   true,
		age:      30 * time.Minute,
		ageKnown: true,
	}
	return h
}

func (h *watchdogHarness) statePath() string { return filepath.Join(h.dir, watchdogStateFile) }

func (h *watchdogHarness) deps() watchdogDeps {
	return watchdogDeps{
		Probe: func(context.Context) error {
			if h.failing {
				return os.ErrDeadlineExceeded
			}
			return nil
		},
		UnitActive: func(context.Context) (bool, error) { return h.active, nil },
		UnitAge:    func(context.Context) (time.Duration, bool) { return h.age, h.ageKnown },
		Restart: func(context.Context) error {
			if h.restartErr != nil {
				return h.restartErr
			}
			h.restarts++
			return nil
		},
		Now: func() time.Time { return h.now },
		Out: &h.out,
	}
}

// step advances the clock by one timer period and runs one watchdog pass.
func (h *watchdogHarness) step(t *testing.T, d time.Duration) watchdogVerdict {
	t.Helper()
	h.now = h.now.Add(d)
	v, err := watchOnce(context.Background(), h.statePath(), h.deps())
	if err != nil {
		t.Fatalf("watchOnce returned an error: %v", err)
	}
	return v
}

func (h *watchdogHarness) state(t *testing.T) watchdogState {
	t.Helper()
	st, ok := loadWatchdogState(h.statePath())
	if !ok {
		t.Fatalf("watchdog state file %s is missing", h.statePath())
	}
	return st
}

// TestWatchdog_ThreeFailuresRestart pins the core contract: two failed probes
// record, the third restarts.
func TestWatchdog_ThreeFailuresRestart(t *testing.T) {
	h := newWatchdogHarness(t)
	const minute = time.Minute

	if v := h.step(t, minute); v != VerdictRecorded {
		t.Fatalf("first failed probe = %q, want %q", v, VerdictRecorded)
	}
	if h.restarts != 0 {
		t.Fatalf("restarted after one failure; want no restart (a single miss is a blip)")
	}
	if v := h.step(t, minute); v != VerdictRecorded {
		t.Fatalf("second failed probe = %q, want %q", v, VerdictRecorded)
	}
	if h.restarts != 0 {
		t.Fatalf("restarted after two failures; want no restart")
	}
	if v := h.step(t, minute); v != VerdictRestarted {
		t.Fatalf("third failed probe = %q, want %q", v, VerdictRestarted)
	}
	if h.restarts != 1 {
		t.Fatalf("restarts = %d, want exactly 1", h.restarts)
	}
	// One line per restart, naming the count: the only line an operator greps.
	if got := h.out.String(); !strings.Contains(got, "restarted the unit after 3 consecutive failed") {
		t.Errorf("restart log line does not name the failure count:\n%s", got)
	}
	st := h.state(t)
	if st.Failures != WatchdogFailuresBeforeRestart {
		t.Errorf("persisted failures = %d, want %d (the count must survive the restart)",
			st.Failures, WatchdogFailuresBeforeRestart)
	}
	if st.LastRestart != h.now.Unix() {
		t.Errorf("persisted last_restart = %d, want %d", st.LastRestart, h.now.Unix())
	}
}

// TestWatchdog_SuccessResetsTheCounter is the other half of N consecutive: two
// misses, then an answer, must leave the count at zero — otherwise a node that
// misses two probes a day would accumulate its way to a restart.
func TestWatchdog_SuccessResetsTheCounter(t *testing.T) {
	h := newWatchdogHarness(t)
	const minute = time.Minute

	h.step(t, minute)
	h.step(t, minute)
	if st := h.state(t); st.Failures != 2 {
		t.Fatalf("failures before recovery = %d, want 2", st.Failures)
	}

	h.failing = false
	if v := h.step(t, minute); v != VerdictHealthy {
		t.Fatalf("successful probe = %q, want %q", v, VerdictHealthy)
	}
	if st := h.state(t); st.Failures != 0 {
		t.Fatalf("failures after one success = %d, want 0 (the counter is CONSECUTIVE)", st.Failures)
	}
	if h.restarts != 0 {
		t.Fatalf("restarts = %d, want 0", h.restarts)
	}
	if got := h.out.String(); !strings.Contains(got, "after 2 consecutive failed probe(s)") {
		t.Errorf("recovery line does not name the count it recovered from:\n%s", got)
	}

	// And the reset is real, not cosmetic: two more failures after it must not
	// reach the threshold.
	h.failing = true
	h.step(t, minute)
	if v := h.step(t, minute); v != VerdictRecorded {
		t.Fatalf("two failures after a reset = %q, want %q (counter did not reset)", v, VerdictRecorded)
	}
	if h.restarts != 0 {
		t.Fatalf("restarts = %d after a reset, want 0", h.restarts)
	}
}

// TestWatchdog_RateLimitsRestarts pins the ten-minute floor. Three failures
// restart once; the next twenty minutes of failure may restart at most twice.
func TestWatchdog_RateLimitsRestarts(t *testing.T) {
	h := newWatchdogHarness(t)
	const minute = time.Minute

	h.step(t, minute)
	h.step(t, minute)
	if v := h.step(t, minute); v != VerdictRestarted {
		t.Fatalf("third failure = %q, want %q", v, VerdictRestarted)
	}

	// Nine more minutes of a still-wedged daemon: no second restart.
	for i := 0; i < 9; i++ {
		if v := h.step(t, minute); v != VerdictRateLimited {
			t.Fatalf("pass %d after a restart = %q, want %q", i+1, v, VerdictRateLimited)
		}
	}
	if h.restarts != 1 {
		t.Fatalf("restarts after 9 minutes = %d, want 1", h.restarts)
	}

	// Past the window, the watchdog acts again — once.
	if v := h.step(t, 2*minute); v != VerdictRestarted {
		t.Fatalf("pass past the rate limit = %q, want %q", v, VerdictRestarted)
	}
	if h.restarts != 2 {
		t.Fatalf("restarts = %d, want 2", h.restarts)
	}
	// The line names the count it saw, so "restarted at 3" and "restarted at
	// 13" are distinguishable in the journal.
	if got := h.out.String(); !strings.Contains(got, "after 13 consecutive failed") {
		t.Errorf("second restart line does not name the accumulated count:\n%s", got)
	}
}

// TestWatchdog_DoesNotTouchAnInactiveUnit: a unit that is not active is
// systemd's business (or an operator's `systemctl stop`). Restarting it would
// make a deliberate stop impossible.
func TestWatchdog_DoesNotTouchAnInactiveUnit(t *testing.T) {
	h := newWatchdogHarness(t)
	h.active = false

	for i := 0; i < 5; i++ {
		if v := h.step(t, time.Minute); v != VerdictInactive {
			t.Fatalf("pass %d with an inactive unit = %q, want %q", i+1, v, VerdictInactive)
		}
	}
	if h.restarts != 0 {
		t.Fatalf("restarts = %d with an inactive unit, want 0", h.restarts)
	}
	if got := h.out.String(); got != "" {
		t.Errorf("an inactive unit wrote to the journal every minute:\n%s", got)
	}
}

// TestWatchdog_LeavesAYoungUnitAlone: a unit that entered `active` less than
// five minutes ago is an upgrade, a manual restart, or this watchdog's own last
// action — never a wedge.
func TestWatchdog_LeavesAYoungUnitAlone(t *testing.T) {
	h := newWatchdogHarness(t)
	h.age = 60 * time.Second

	// The count still accrues — the age guard is a bound on the RESTART, not on
	// the measurement. Two passes record, the third is where the age matters.
	for i := 0; i < 2; i++ {
		if v := h.step(t, time.Minute); v != VerdictRecorded {
			t.Fatalf("pass %d with a 1-minute-old unit = %q, want %q", i+1, v, VerdictRecorded)
		}
	}
	for i := 0; i < 3; i++ {
		if v := h.step(t, time.Minute); v != VerdictYoungUnit {
			t.Fatalf("pass %d with a 1-minute-old unit = %q, want %q", i+3, v, VerdictYoungUnit)
		}
	}
	if h.restarts != 0 {
		t.Fatalf("restarts = %d for a barely-started unit, want 0", h.restarts)
	}

	// The same failure count DOES restart once the unit is old enough: the guard
	// is an age test, not a mute button.
	h.age = 6 * time.Minute
	if v := h.step(t, time.Minute); v != VerdictRestarted {
		t.Fatalf("pass with a 6-minute-old unit = %q, want %q", v, VerdictRestarted)
	}
	if h.restarts != 1 {
		t.Fatalf("restarts = %d, want 1", h.restarts)
	}
}

// TestWatchdog_UnknownUnitAgeDoesNotRestart: an age we cannot read must never
// be read as "old enough". The conservative answer is to do nothing.
func TestWatchdog_UnknownUnitAgeDoesNotRestart(t *testing.T) {
	h := newWatchdogHarness(t)
	h.ageKnown = false
	for i := 0; i < 4; i++ {
		h.step(t, time.Minute)
	}
	if h.restarts != 0 {
		t.Fatalf("restarts = %d with an unknown unit age, want 0", h.restarts)
	}
}

// TestWatchdog_StateSurvivesTheFilesystem: the counter is re-read, not cached in
// a process — a restart of the probe command itself must not reset it.
func TestWatchdog_StateSurvivesTheFilesystem(t *testing.T) {
	h := newWatchdogHarness(t)
	h.step(t, time.Minute)
	h.step(t, time.Minute)

	raw, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var disk watchdogState
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatalf("state file is not JSON: %v\n%s", err, raw)
	}
	if disk.Failures != 2 {
		t.Fatalf("state file failures = %d, want 2", disk.Failures)
	}
	// A fresh harness on the same dir (a new process) continues the count.
	next := &watchdogHarness{
		dir:      h.dir,
		now:      h.now.Add(time.Minute),
		failing:  true,
		active:   true,
		age:      30 * time.Minute,
		ageKnown: true,
	}
	if v, err := watchOnce(context.Background(), h.statePath(), next.deps()); err != nil || v != VerdictRestarted {
		t.Fatalf("a new process on the same state = %q (err %v), want %q", v, err, VerdictRestarted)
	}
}

// TestProbeHealth_OnlyA2xxIsHealth: the daemon answers /health unauthenticated
// and ungated, so a non-2xx means something OTHER than the daemon holds the port.
func TestProbeHealth_OnlyA2xxIsHealth(t *testing.T) {
	for _, tc := range []struct {
		code int
		ok   bool
	}{{200, true}, {204, true}, {302, false}, {404, false}, {503, false}} {
		code := tc.code
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		err := probeHealth(context.Background(), srv.URL)
		srv.Close()
		if tc.ok && err != nil {
			t.Errorf("%d: probeHealth = %v, want nil", code, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%d: probeHealth = nil, want an error", code)
		}
	}
}

func TestProbeHealth_RefusedConnectionIsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listens now
	if err := probeHealth(context.Background(), url); err == nil {
		t.Fatal("probing a closed port returned nil; a dead daemon must fail the probe")
	}
}

// TestHostLoopbackURL: serve binds 0.0.0.0, and dialing the unspecified address
// is not a probe of this node.
func TestHostLoopbackURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"0.0.0.0:9090", "http://127.0.0.1:9090/health"},
		{"127.0.0.1:9090", "http://127.0.0.1:9090/health"},
		{"[::]:9090", "http://[::1]:9090/health"},
		{":9090", "http://127.0.0.1:9090/health"},
		{"", defaultWatchdogProbeURL},
		{"garbage", defaultWatchdogProbeURL},
	} {
		if got := hostLoopbackURL(tc.in); got != tc.want {
			t.Errorf("hostLoopbackURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestWatchdogUnits_AreWiredEveryMinute pins the unit wiring the decision table
// depends on: a 60 s timer, a oneshot service, and the three arguments the
// command needs to work as root without reading the operator's config.
func TestWatchdogUnits_AreWiredEveryMinute(t *testing.T) {
	timer := unitSections(t, renderWatchdogTimer())
	tim, ok := timer["Timer"]
	if !ok {
		t.Fatalf("timer has no [Timer] section:\n%s", renderWatchdogTimer())
	}
	if got := unitSeconds(t, "OnUnitActiveSec", tim["OnUnitActiveSec"]); got != time.Minute {
		t.Errorf("OnUnitActiveSec = %s, want 1m0s", got)
	}
	if tim["Unit"] != "pmcluster-watchdog.service" {
		t.Errorf("Timer.Unit = %q, want pmcluster-watchdog.service", tim["Unit"])
	}
	if tim["Persistent"] != "" {
		t.Error("Persistent must not be set: a run missed while the node was down has nothing to probe")
	}

	svc := unitSections(t, renderWatchdogService(
		"/usr/local/bin/pmcluster", "/home/pmuser/.pmcluster/watchdog.json", defaultWatchdogProbeURL))
	s, ok := svc["Service"]
	if !ok {
		t.Fatalf("service has no [Service] section")
	}
	if s["Type"] != "oneshot" {
		t.Errorf("Type = %q, want oneshot", s["Type"])
	}
	if s["User"] != "root" {
		t.Errorf("User = %q, want root (the only user that may restart the pmcluster unit)", s["User"])
	}
	for _, want := range []string{
		"health-watch",
		"--state-file /home/pmuser/.pmcluster/watchdog.json",
		"--probe-url " + defaultWatchdogProbeURL,
		"--unit pmcluster",
	} {
		if !strings.Contains(s["ExecStart"], want) {
			t.Errorf("ExecStart is missing %q:\n%s", want, s["ExecStart"])
		}
	}
	// A watchdog with a dependency on the thing it watches is not a watchdog.
	for _, forbidden := range []string{"BindsTo", "Requires"} {
		if _, bad := svc["Unit"][forbidden]; bad {
			t.Errorf("[Unit] %s pulls the watchdog down with docker; it must outlive it", forbidden)
		}
	}
}

// TestEnsureDaemonRunning_InstallsTheWatchdog: the units in contrib/ are only a
// reference. What makes the watchdog exist on a node is this wiring, next to the
// daemon unit, with the timer enabled — a watchdog that is written but never
// enabled is the failure mode this test names.
func TestEnsureDaemonRunning_InstallsTheWatchdog(t *testing.T) {
	oldPath, oldSys, oldHas, oldWrite, oldHost := daemonUnitPath, systemctlFn, hasSystemctl, writeUnitFn, hostOS
	t.Cleanup(func() {
		daemonUnitPath, systemctlFn, hasSystemctl, writeUnitFn, hostOS = oldPath, oldSys, oldHas, oldWrite, oldHost
	})
	hostOS = "linux"
	hasSystemctl = func() bool { return true }
	writeUnitFn = os.WriteFile
	t.Setenv("PMCLUSTER_USER", "testuser")

	dir := t.TempDir()
	daemonUnitPath = filepath.Join(dir, "pmcluster.service")
	var calls []string
	systemctlFn = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}

	var out bytes.Buffer
	if err := ensureDaemonRunning(&out); err != nil {
		t.Fatalf("ensureDaemonRunning: %v", err)
	}

	servicePath, timerPath := watchdogUnitPaths()
	svc, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatalf("watchdog service not written next to the daemon unit: %v", err)
	}
	if !strings.Contains(string(svc), "--state-file ") || !strings.Contains(string(svc), "/.pmcluster/watchdog.json") {
		t.Errorf("watchdog service does not point at the daemon's data dir:\n%s", svc)
	}
	if strings.Contains(string(svc), filepath.Join(dir, "watchdog.json")) {
		t.Errorf("watchdog state file is in the unit directory, not the data dir:\n%s", svc)
	}
	if _, err := os.ReadFile(timerPath); err != nil {
		t.Fatalf("watchdog timer not written: %v", err)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{"enable pmcluster-watchdog.timer", "restart pmcluster-watchdog.timer"} {
		if !strings.Contains(joined, want) {
			t.Errorf("systemctl calls missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(out.String(), "health watchdog active") {
		t.Errorf("output does not report the watchdog:\n%s", out.String())
	}

	// Idempotent: a second pass must not rewrite the units.
	var out2 bytes.Buffer
	if err := ensureDaemonRunning(&out2); err != nil {
		t.Fatalf("second ensureDaemonRunning: %v", err)
	}
	if strings.Contains(out2.String(), "installed health watchdog units") {
		t.Errorf("second run re-installed the watchdog units:\n%s", out2.String())
	}
}
