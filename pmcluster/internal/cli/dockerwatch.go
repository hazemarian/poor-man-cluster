package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// docker-watch: the one thing pmcluster cannot do for itself.
//
// The daemon owns the node's control loop, so when IT is wedged systemd or
// pmcluster-watchdog's /health probe covers it. The docker daemon is outside that
// loop: pmcluster can restart itself, but not dockerd. On 2026-11-12 nxt-sw-3-w went
// down with dockerd gone — ports 2377/7946 closed, sshd unable to complete a
// handshake, kernel still answering ping. Nothing on the node was watching dockerd,
// and the swarm saw a node that stayed Down until a human power-cycled it.
//
// So the same root timer that probes /health also probes the docker socket when the
// operator enables it. It is a SEPARATE check with its own state file and its own
// decision table, because dockerd fails differently from a wedged pmcluster:
//
//   - a HUNG dockerd leaves the unit `active`; restarting it is the fix.
//   - a KILLED dockerd often hits systemd's StartLimitBurst, which leaves the unit
//     `failed` and stops systemd from trying again. Restarting is not enough there:
//     `reset-failed` is what re-arms the unit. This is the case that took a node out
//     of the cluster for hours, and it is the reason this check exists at all.
//
// The danger is real and is why it ships off by default: restarting docker kills
// every container on that node, including a running CI job and the platform services
// scheduled there. So it is gated on the failure persisting, and rate-limited far
// harder than the pmcluster restart (30 minutes against 10): a dockerd that dies
// three times in half an hour is a bug to investigate, not a service to keep bouncing.

const (
	// DockerWatchFailuresBeforeRestart is 3 consecutive failed socket probes, one
	// minute apart. One failure is a busy node; three is a dead daemon.
	DockerWatchFailuresBeforeRestart = 3
	// DockerWatchRestartFloor is the minimum gap between two docker restarts. It is
	// three times the pmcluster floor because the blast radius is three times larger.
	DockerWatchRestartFloor = 30 * time.Minute
	// DockerWatchProbeTimeout bounds the socket probe.
	DockerWatchProbeTimeout = 5 * time.Second
	// dockerWatchStateFile is persisted separately from the pmcluster counter: the
	// two failures mean different things and must not share a rate limit.
	dockerWatchStateFile = "watchdog-docker.json"
	// defaultDockerSocket is where dockerd listens on every node this platform runs.
	defaultDockerSocket = "/var/run/docker.sock"
	// nodePolicyFileName is the document the leader's node-level decisions reach this
	// node through. Written by the runner agent (worker) or the daemon (manager), read
	// by the watchdog.
	nodePolicyFileName = "node-policy.json"
)

// dockerVerdict is what one docker-watch run decided.
type dockerVerdict string

const (
	DockerVerdictHealthy     dockerVerdict = "healthy"
	DockerVerdictRecorded    dockerVerdict = "failure-recorded"
	DockerVerdictRestarted   dockerVerdict = "restarted"
	DockerVerdictReArmed     dockerVerdict = "reset-failed-and-restarted"
	DockerVerdictRateLimited dockerVerdict = "rate-limited"
)

// dockerWatchDeps are the seams; every side effect is injectable so the decision
// table is testable without systemd, docker, or a sleeping test.
type dockerWatchDeps struct {
	// Probe returns nil when the docker socket answered.
	Probe func(ctx context.Context) error
	// UnitActive reports whether systemd says docker.service is active.
	UnitActive func(ctx context.Context) (bool, error)
	// Restart restarts docker.service.
	Restart func(ctx context.Context) error
	// ResetFailed clears systemd's start-limit state so a restart is actually
	// attempted. Best effort: a unit that is not failed reports nothing.
	ResetFailed func(ctx context.Context) error
	// Out is the journal writer (the timer's stdout).
	Out io.Writer
	Now func() time.Time
}

func (d dockerWatchDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// healDeps are the seams for completing a docker restart: the unit that docker's
// restart can take down with it, and the three systemd verbs needed to bring it back.
type healDeps struct {
	UnitActive  func(ctx context.Context) (bool, error)
	ResetFailed func(ctx context.Context) error
	Start       func(ctx context.Context) error
	Out         io.Writer
}

// healControlPlaneAfterDockerRestart brings this node's daemon back when the docker
// restart this run just performed left it down.
//
// Restarting docker.service propagates to pmcluster through `PartOf=` (docker.service
// lists it in `ConsistsOf=`), so the watchdog's own remedy is a thing that stops the
// control plane. It is also the actor that knows it just did so, which no general
// babysitter could: an inactive unit is normally systemd's business and an operator's
// decision. Measured on nxt-sw-6-w (2026-11-13): docker came back, pmcluster did not —
// five starts inside the unit's five-minute window had already been spent by the kills,
// so systemd answered "start request repeated too quickly" and left it `failed`, and a
// `failed` unit is not started again by anything when docker returns.
//
// reset-failed FIRST for the same reason the docker path needs it: a start-limited unit
// refuses a start, and that refusal is silent.
func healControlPlaneAfterDockerRestart(ctx context.Context, d healDeps) (bool, error) {
	out := d.Out
	if out == nil {
		out = io.Discard
	}
	if active, err := d.UnitActive(ctx); err == nil && active {
		return false, nil // nothing to do: the restart did not take it down
	}
	// Best effort: on a unit that is merely inactive (not start-limited) this is a
	// no-op, and a refusal here must not stop the start below.
	if d.ResetFailed != nil {
		if err := d.ResetFailed(ctx); err != nil {
			fmt.Fprintf(out, "pmcluster-watchdog: reset-failed %s: %v (continuing to the start)\n", dockerWatchUnitName, err)
		}
	}
	if err := d.Start(ctx); err != nil {
		return true, fmt.Errorf("start %s after the docker restart: %w", dockerWatchUnitName, err)
	}
	fmt.Fprintf(out, "%s pmcluster-watchdog: %s was down after the docker restart; started it\n",
		time.Now().UTC().Format(time.RFC3339), dockerWatchUnitName)
	return true, nil
}

// dockerWatchUnitName is the daemon unit the watchdog heals; a constant so a test can
// assert the log line without reaching into the command wiring.
const dockerWatchUnitName = "pmcluster"

// watchDockerOnce is one tick of the docker check.
func watchDockerOnce(ctx context.Context, statePath string, d dockerWatchDeps) (dockerVerdict, error) {
	out := d.Out
	if out == nil {
		out = io.Discard
	}
	now := d.now()
	st, _ := loadWatchdogState(statePath)

	if err := d.Probe(ctx); err == nil {
		if st.Failures > 0 {
			fmt.Fprintf(out, "%s pmcluster-watchdog: docker answers again after %d consecutive failed probe(s); counter reset\n",
				now.UTC().Format(time.RFC3339), st.Failures)
			st.Failures = 0
			st.LastProbe = now.Unix()
			_ = saveWatchdogState(statePath, st)
		}
		return DockerVerdictHealthy, nil
	} else {
		st.Failures++
		st.LastProbe = now.Unix()
		st.LastFailure = now.Unix()
		fmt.Fprintf(out, "%s pmcluster-watchdog: docker did not answer probe %d/%d: %v\n",
			now.UTC().Format(time.RFC3339), st.Failures, DockerWatchFailuresBeforeRestart, err)
	}

	if st.Failures < DockerWatchFailuresBeforeRestart {
		_ = saveWatchdogState(statePath, st)
		return DockerVerdictRecorded, nil
	}

	if st.LastRestart > 0 && now.Sub(time.Unix(st.LastRestart, 0)) < DockerWatchRestartFloor {
		_ = saveWatchdogState(statePath, st)
		fmt.Fprintf(out, "%s pmcluster-watchdog: docker is still down, but it was restarted %s ago (floor %s); not restarting again\n",
			now.UTC().Format(time.RFC3339),
			now.Sub(time.Unix(st.LastRestart, 0)).Round(time.Minute), DockerWatchRestartFloor)
		return DockerVerdictRateLimited, nil
	}

	verdict := DockerVerdictRestarted
	if active, err := d.UnitActive(ctx); err == nil && !active {
		// The killed-daemon case. systemd has stopped trying (StartLimitBurst), so a
		// plain restart would be refused: the failed state has to be cleared first.
		verdict = DockerVerdictReArmed
		if err := d.ResetFailed(ctx); err != nil {
			fmt.Fprintf(out, "%s pmcluster-watchdog: reset-failed docker: %v (continuing to the restart)\n",
				now.UTC().Format(time.RFC3339), err)
		}
	}

	fmt.Fprintf(out, "%s pmcluster-watchdog: restarting docker after %d consecutive failed probes — EVERY container on this node will stop\n",
		now.UTC().Format(time.RFC3339), st.Failures)
	if err := d.Restart(ctx); err != nil {
		st.LastRestart = now.Unix()
		_ = saveWatchdogState(statePath, st)
		return verdict, fmt.Errorf("restart docker: %w", err)
	}

	// Verify instead of assuming: a restart that did not produce a live daemon is a
	// node that needs an operator, and saying so is the whole value of this check.
	if err := d.Probe(ctx); err != nil {
		st.LastRestart = now.Unix()
		st.Failures = 0
		_ = saveWatchdogState(statePath, st)
		fmt.Fprintf(out, "%s pmcluster-watchdog: docker STILL does not answer after a restart (%v) — this node needs an operator\n",
			now.UTC().Format(time.RFC3339), err)
		return verdict, fmt.Errorf("docker did not answer after a restart: %w", err)
	}

	fmt.Fprintf(out, "%s pmcluster-watchdog: docker answers again after the restart\n", now.UTC().Format(time.RFC3339))
	st.Failures = 0
	st.LastRestart = now.Unix()
	_ = saveWatchdogState(statePath, st)
	return verdict, nil
}

// nodePolicyArmsDocker reads the leader's decision from the node policy document.
//
// Anything unreadable, missing or malformed means NOT armed: this decides whether a
// root timer restarts a node's docker daemon, and every failure mode must fall towards
// doing nothing.
func nodePolicyArmsDocker(path string) bool {
	if path == "" {
		return false
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var doc struct {
		DockerRestart bool `json:"docker_restart"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return false
	}
	return doc.DockerRestart
}

// probeDockerSocket asks the daemon for /_ping over its unix socket. Using the API
// rather than the CLI keeps the watchdog independent of every workflow's docker
// binary and of DOCKER_HOST being set in a root timer's environment.
func probeDockerSocket(ctx context.Context, socketPath string) error {
	if socketPath == "" {
		socketPath = defaultDockerSocket
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/_ping", nil)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: tr, Timeout: DockerWatchProbeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker /_ping returned %s", resp.Status)
	}
	return nil
}
