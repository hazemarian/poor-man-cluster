package nodeunits

import (
	"fmt"
	"path/filepath"
	"strings"
)

// WatchdogStateFile lives under the daemon's data dir, NOT in /run: the failure
// count has to survive the restart it triggers, or every restart would reset the
// count to zero and the watchdog would restart once a minute instead of once per
// rate limit. It is named here because the unit that runs the watchdog and the
// runtime that writes the counter have to agree.
const WatchdogStateFile = "watchdog.json"

// WatchdogFailuresBeforeRestart is N: three consecutive failed probes, one minute
// apart. One failure is a blip (a GC pause, a busy CPU during a build); three is a
// minute of silence, which a healthy daemon cannot do.
const WatchdogFailuresBeforeRestart = 3

// NodePolicyFileName is the document the leader's node-level decisions reach this
// node in (today: whether docker may be restarted). It sits beside the watchdog's
// state file, so the unit that reads it and the code that writes it agree on one
// name.
const NodePolicyFileName = "node-policy.json"

// DefaultWatchdogProbeURL is what the timer probes. 127.0.0.1, not the configured
// listen address: `serve` binds 0.0.0.0, and dialing the unspecified address is not
// a probe of this node.
const DefaultWatchdogProbeURL = "http://127.0.0.1:9090/health"

// The health watchdog is the only actor that can restart a daemon that is alive and
// no longer answering, and its unit is where its CAPABILITY is decided: the docker
// socket is what it can reach, the policy file is what the leader said. Both flags
// are asserted in tests, so the text lives here rather than beside the command that
// installs it.

// renderWatchdogTimer builds pmcluster-watchdog.timer: every 60 seconds, with
// the first run a minute and a half after boot (long enough for a slow daemon to
// bind, short enough that a node which boots wedged is fixed without a login).
func RenderWatchdogTimer() string {
	return `[Unit]
Description=pmcluster health watchdog (every minute)

[Timer]
OnBootSec=90s
OnUnitActiveSec=60s
AccuracySec=5s
Unit=pmcluster-watchdog.service

[Install]
WantedBy=timers.target
`
}

// renderWatchdogServiceWithDocker adds the docker check when dockerSocket is set.
//
// It is separate from the /health probe because the two watch different processes
// with different remedies, and because restarting docker stops every container on the
// node — an operator turns that on deliberately, never by upgrading.
func RenderWatchdogServiceWithDocker(execStart, statePath, probeURL, dockerSocket string) string {
	// The socket is the CAPABILITY and the policy file is the PERMISSION. They are
	// separate on purpose: a worker has no store to read `node_docker_restart` from,
	// so the leader's answer reaches it through the policy document its agent writes.
	// Gating the unit on a setting instead meant the feature silently did nothing on
	// exactly the nodes that needed it.
	dockerFlag := ""
	if s := strings.TrimSpace(dockerSocket); s != "" {
		policy := filepath.Join(filepath.Dir(statePath), NodePolicyFileName)
		dockerFlag = " --docker-socket " + s + " --policy-file " + policy
	}
	return fmt.Sprintf(`[Unit]
Description=pmcluster health watchdog (restarts a wedged daemon)
# A watchdog that is rate-limited by systemd is a watchdog that stops watching:
# ten starts per five minutes is above the one-per-ten-minutes floor the
# command enforces, so this limit only ever catches a stuck unit.
StartLimitIntervalSec=300
StartLimitBurst=10

[Service]
Type=oneshot
User=root
ExecStart=%s health-watch --state-file %s --probe-url %s --unit pmcluster%s
# A probe is 5s and a restart is bounded by systemd; the ceiling exists so a
# hung systemctl cannot stack timer runs on top of each other.
TimeoutStartSec=120
# The control plane's own monitor yields to the workload it is watching.
Nice=5
`, execStart, statePath, probeURL, dockerFlag)
}

// renderWatchdogService builds pmcluster-watchdog.service: one oneshot run of
// the hidden health-watch subcommand, driven by the timer below.
//
// Root, because the one thing it does is `systemctl restart pmcluster`. The
// probe URL, the unit name and the state file are all passed explicitly: the
// watchdog must not need to read the operator's config (it runs as root, and a
// root process guessing at another user's data dir is how the wrong state file
// gets written).
//
// No After=/BindsTo=docker.service, unlike the daemon unit: a wedge is often
// caused by exactly the thing a dependency ordering would wait for.
func RenderWatchdogService(execStart, statePath, probeURL string) string {
	return RenderWatchdogServiceWithDocker(execStart, statePath, probeURL, "")
}
