package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
)

// The health watchdog: the one actor that can bring a WEDGED daemon back.
//
// Why it exists. systemd's Restart=always covers a process that EXITS. It does
// nothing for a process that is still alive and no longer answering: a daemon
// whose loop is stuck on a lock, or whose runtime ran out of memory and is
// thrashing, stays `active (running)` forever while `/health` times out. On
// 2026-11-10 two nodes were OOM-killed by a CI build; sshd died with them and
// nothing could be restarted remotely — the control plane ran with 2 of 3
// managers for hours. The watchdog is the in-host actor that needs no login:
// a timer, a probe, and a bounded restart.
//
// What it is NOT: a supervisor that fights systemd. It restarts the unit only
// when the unit is ACTIVE, the probe failed N consecutive times, the unit has
// been up longer than five minutes, and it has not restarted it in the last
// ten. Every one of those bounds exists because the opposite is a machine that
// reboots its own control plane in a loop: a standby daemon that is briefly
// unreachable during an upgrade, a node whose daemon just started pulling its
// first state, or a probe that fails because the machine is ALREADY restarting.

const (
	// WatchdogFailuresBeforeRestart is N: three consecutive failed probes, one
	// minute apart. One failure is a blip (a GC pause, a busy CPU during a
	// build); three is a minute of silence, which a healthy daemon cannot do.
	WatchdogFailuresBeforeRestart = 3

	// WatchdogRestartRateLimit is the floor between two restarts. A daemon that
	// cannot answer 10 minutes after being restarted will not be fixed by being
	// restarted again, and a restart every minute would make the journal useless
	// at exactly the moment somebody is reading it.
	WatchdogRestartRateLimit = 10 * time.Minute

	// WatchdogMinUnitAge is how long the unit must have been up before a
	// restart is allowed. A unit that entered `active` a minute ago is either
	// an upgrade, a deliberate `systemctl restart`, or this watchdog's own last
	// action — restarting it again would turn a slow start into a loop.
	WatchdogMinUnitAge = 5 * time.Minute

	// WatchdogProbeTimeout bounds the probe. A daemon that has not answered in
	// five seconds is not answering; waiting longer only delays the decision.
	WatchdogProbeTimeout = 5 * time.Second

	// watchdogStateFile lives under the daemon's data dir, NOT in /run: the
	// failure count has to survive the restart it triggers, or every restart
	// would reset the count to zero and the watchdog would restart once a
	// minute instead of once per rate limit.
	watchdogStateFile = "watchdog.json"

	// defaultWatchdogProbeURL is what the timer probes. 127.0.0.1, not the
	// configured listen address: `serve` binds 0.0.0.0, and dialing the
	// unspecified address is not a probe of this node.
	defaultWatchdogProbeURL = "http://127.0.0.1:9090/health"
)

// watchdogState is the persisted evidence, one small JSON file.
type watchdogState struct {
	// Failures is the CONSECUTIVE failed-probe count. It is reset by one
	// successful probe and by nothing else — in particular not by a restart.
	Failures int `json:"failures"`
	// LastRestart is when this watchdog last restarted the unit (unix, 0 =
	// never). It is what the rate limit reads.
	LastRestart int64 `json:"last_restart,omitempty"`
	// LastProbe and LastFailure are for the operator reading the file.
	LastProbe   int64 `json:"last_probe,omitempty"`
	LastFailure int64 `json:"last_failure,omitempty"`
}

// watchdogVerdict is what one run decided. It is a string so a test asserts the
// decision instead of counting log lines.
type watchdogVerdict string

const (
	// VerdictHealthy: the probe answered; the counter is zero.
	VerdictHealthy watchdogVerdict = "healthy"
	// VerdictRecorded: the probe failed and the count is below N.
	VerdictRecorded watchdogVerdict = "failure-recorded"
	// VerdictRestarted: the probe failed N times and the unit was restarted.
	VerdictRestarted watchdogVerdict = "restarted"
	// VerdictInactive: the probe failed but the unit is not active, so systemd
	// (or an operator) owns the situation and the watchdog stays out.
	VerdictInactive watchdogVerdict = "unit-not-active"
	// VerdictYoungUnit: the unit entered active too recently — an upgrade, a
	// manual restart, or this watchdog's own last action.
	VerdictYoungUnit watchdogVerdict = "unit-too-young"
	// VerdictRateLimited: a restart happened less than the rate limit ago.
	VerdictRateLimited watchdogVerdict = "rate-limited"
)

// watchdogDeps are the seams: every side effect is injectable, so the decision
// table is testable without systemd, docker, or a sleeping test.
type watchdogDeps struct {
	// Probe returns nil when the daemon answered, an error otherwise.
	Probe func(ctx context.Context) error
	// UnitActive reports whether systemd says the unit is active (running).
	UnitActive func(ctx context.Context) (bool, error)
	// UnitAge reports how long the unit has been in `active`. known=false when
	// it cannot be determined, which refuses the restart: an unknown age must
	// never be read as "old enough".
	UnitAge func(ctx context.Context) (age time.Duration, known bool)
	// Restart restarts the unit.
	Restart func(ctx context.Context) error
	// Now is the clock.
	Now func() time.Time
	// Out receives the restart line (journald captures it).
	Out io.Writer
}

func (d watchdogDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// watchOnce is the whole decision: probe, count, and restart only inside all
// four bounds. It is idempotent and safe to run every minute.
func watchOnce(ctx context.Context, statePath string, d watchdogDeps) (watchdogVerdict, error) {
	out := d.Out
	if out == nil {
		out = io.Discard
	}
	now := d.now()
	st, _ := loadWatchdogState(statePath)

	if err := d.Probe(ctx); err == nil {
		if st.Failures > 0 {
			fmt.Fprintf(out, "%s pmcluster-watchdog: /health answers again after %d consecutive failed probe(s); counter reset\n",
				now.UTC().Format(time.RFC3339), st.Failures)
		}
		st.Failures = 0
		st.LastProbe = now.Unix()
		_ = saveWatchdogState(statePath, st)
		return VerdictHealthy, nil
	}

	st.Failures++
	st.LastProbe = now.Unix()
	st.LastFailure = now.Unix()
	// Persist BEFORE acting. If the restart is what kills this process, the
	// count still describes what happened.
	_ = saveWatchdogState(statePath, st)

	active, err := d.UnitActive(ctx)
	if err != nil {
		// Cannot ask systemd → do nothing. Guessing here means restarting a
		// unit that may already be down, which systemd would then fight.
		return VerdictInactive, fmt.Errorf("ask systemd whether pmcluster is active: %w", err)
	}
	if !active {
		// The unit is inactive/failed/deactivating: that is systemd's state
		// machine or an operator's decision, not a wedge. Restarting would
		// turn `systemctl stop pmcluster` into a fight.
		return VerdictInactive, nil
	}
	if st.Failures < WatchdogFailuresBeforeRestart {
		return VerdictRecorded, nil
	}
	if age, known := d.UnitAge(ctx); known && age < WatchdogMinUnitAge {
		return VerdictYoungUnit, nil
	} else if !known {
		// Unknown age is not "old enough": a restart we cannot justify is the
		// loop this whole file is written to avoid.
		return VerdictYoungUnit, nil
	}
	if st.LastRestart != 0 && now.Sub(time.Unix(st.LastRestart, 0)) < WatchdogRestartRateLimit {
		return VerdictRateLimited, nil
	}

	if err := d.Restart(ctx); err != nil {
		// The counter stays where it is: the next run retries, still bounded by
		// the rate limit, and the failure is in the journal with its reason.
		return "", fmt.Errorf("restart pmcluster after %d failed health probes: %w", st.Failures, err)
	}
	st.LastRestart = now.Unix()
	_ = saveWatchdogState(statePath, st)
	// ONE line per restart, naming the count: this is the line an operator
	// greps for when asking "did anything restart my daemon overnight?".
	fmt.Fprintf(out, "%s pmcluster-watchdog: restarted the unit after %d consecutive failed /health probes (last restart %s)\n",
		now.UTC().Format(time.RFC3339), st.Failures, lastRestartWords(st.LastRestart, now))
	return VerdictRestarted, nil
}

// lastRestartWords renders "never" or "12m0s ago" for the restart line.
func lastRestartWords(at int64, now time.Time) string {
	if at <= 0 {
		return "never"
	}
	return now.Sub(time.Unix(at, 0)).Round(time.Second).String() + " ago"
}

// loadWatchdogState reads the state file. A missing or corrupt file is an empty
// state, never an error: the watchdog must still work on a node whose data dir
// was just created, and a corrupt counter costs at most one extra minute of
// silence.
func loadWatchdogState(path string) (watchdogState, bool) {
	var st watchdogState
	if strings.TrimSpace(path) == "" {
		return st, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return st, false
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return watchdogState{}, false
	}
	return st, true
}

// saveWatchdogState writes the state 0600 via a rename, so a crash mid-write
// cannot leave a half-written counter that reads as zero.
func saveWatchdogState(path string, st watchdogState) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create watchdog state dir: %w", err)
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write watchdog state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace watchdog state: %w", err)
	}
	return nil
}

// probeHealth GETs the probe URL with a bounded timeout and accepts only a 2xx.
//
// A 3xx/4xx is NOT health: the daemon answers /health unauthenticated and
// ungated (adoptgate exempts it), so anything other than 2xx means something
// other than the daemon answered — a proxy on the port, or a listener that is
// not this process.
func probeHealth(ctx context.Context, url string) error {
	c, cancel := context.WithTimeout(ctx, WatchdogProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s answered %d", url, resp.StatusCode)
	}
	return nil
}

// unitActiveNow asks systemd. `is-active` prints the state and exits non-zero
// for anything but active, so the OUTPUT is the answer, not the exit code.
func unitActiveNow(ctx context.Context, unit string) (bool, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "is-active", unit).Output()
	state := strings.TrimSpace(string(out))
	if state == "" && err != nil {
		return false, fmt.Errorf("systemctl is-active %s: %w", unit, err)
	}
	return state == "active", nil
}

// unitAgeNow derives how long the unit has been active from two clocks that need
// no locale, no timezone and no date parsing:
//
//	ActiveEnterTimestampMonotonic — microseconds since boot when it entered active
//	/proc/uptime                  — seconds since boot
//
// age = uptime - monotonic. A unit that never started reports 0, which is
// reported as unknown rather than as "decades old".
func unitAgeNow(ctx context.Context, unit string) (time.Duration, bool) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", unit,
		"-p", "ActiveEnterTimestampMonotonic", "--value").Output()
	if err != nil {
		return 0, false
	}
	us, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if perr != nil || us <= 0 {
		return 0, false
	}
	b, uerr := os.ReadFile("/proc/uptime")
	if uerr != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, false
	}
	up, ferr := strconv.ParseFloat(fields[0], 64)
	if ferr != nil {
		return 0, false
	}
	age := up - float64(us)/1e6
	if age < 0 {
		// Clock skew between the two sources: treat as unknown rather than as
		// a negative age that would pass the minimum-age test by accident.
		return 0, false
	}
	return time.Duration(age * float64(time.Second)), true
}

// restartUnit restarts the daemon through systemd. It is deliberately the same
// systemctl path `cluster update` uses, so a restart from the timer and one from
// the CLI look identical in the journal.
func restartUnit(ctx context.Context, unit string) error {
	cmd := exec.CommandContext(ctx, "systemctl", "restart", unit)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart %s: %w (%s)", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// resetFailedUnit clears systemd's start-limit state for a unit it has given up on.
//
// This is the half that matters for a KILLED dockerd: an OOM kill that repeats in
// quick succession trips StartLimitBurst, after which systemd refuses to try again
// and leaves the unit `failed`. `systemctl restart` is then a no-op with an error
// nobody reads, and the node sits there looking recoverable. reset-failed re-arms it.
func resetFailedUnit(ctx context.Context, unit string) error {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "systemctl", "reset-failed", unit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl reset-failed %s: %w (%s)", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// watchdogFlags are the hidden command's inputs. The unit sets both explicitly,
// so a root-run timer neither reads nor depends on the operator's config file.
var (
	watchdogStatePath string
	watchdogProbeURL  string
	watchdogUnit      string
	// watchdogDockerSocket is empty by default: watching docker is a deliberate
	// choice because its remedy restarts every container on the node.
	watchdogDockerSocket string
	// watchdogPolicyFile is the node policy document the leader's decision reaches
	// this node through. Empty means "no policy": not armed.
	watchdogPolicyFile string
)

var healthWatchCmd = &cobra.Command{
	Use:   "health-watch",
	Short: "Probe the local daemon and restart it when it is wedged (run by a timer)",
	Long: `Probe http://127.0.0.1:9090/health and restart the pmcluster unit when the
daemon is WEDGED — alive, but no longer answering.

This is the one thing systemd's Restart=always cannot do: it covers a process
that exits, not one that hangs. A restart happens only when ALL of these hold:

  - the unit is ` + "`active`" + ` (an inactive or failed unit is systemd's business
    and an operator's decision, never the watchdog's);
  - the probe failed ` + "3" + ` consecutive times (~3 minutes);
  - the unit entered ` + "`active`" + ` more than ` + "5" + ` minutes ago (an upgrade, a manual
    restart, or this watchdog's own last action is not a wedge);
  - the previous restart was more than ` + "10" + ` minutes ago.

The failure count lives in <data_dir>/watchdog.json, so the restart it triggers
does not reset it to zero.

Run by pmcluster-watchdog.timer every minute; installed by 'pmcluster cluster
update'. Hidden because it is not an operator command.`,
	Hidden: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		statePath := watchdogStatePath
		probeURL := watchdogProbeURL
		// The config is read only when a default is actually needed: the unit
		// passes both paths explicitly, so a root-run timer never depends on
		// being able to read the operator's config file.
		needConfig := statePath == "" || !cmd.Flags().Changed("probe-url")
		if needConfig {
			cfg, err := config.Load(configPath)
			if err != nil {
				return fmt.Errorf("resolve the data dir for the watchdog state: %w", err)
			}
			if statePath == "" {
				statePath = filepath.Join(cfg.DataDir, watchdogStateFile)
			}
			if !cmd.Flags().Changed("probe-url") {
				probeURL = hostLoopbackURL(cfg.ListenAddr)
			}
		}
		unit := watchdogUnit
		ctx := cmd.Context()

		verdict, err := watchOnce(ctx, statePath, watchdogDeps{
			Probe:      func(ctx context.Context) error { return probeHealth(ctx, probeURL) },
			UnitActive: func(ctx context.Context) (bool, error) { return unitActiveNow(ctx, unit) },
			UnitAge:    func(ctx context.Context) (time.Duration, bool) { return unitAgeNow(ctx, unit) },
			Restart:    func(ctx context.Context) error { return restartUnit(ctx, unit) },
			Out:        cmd.OutOrStdout(),
		})
		// The docker check runs SECOND and its failure never hides the daemon verdict:
		// the node can have a healthy control plane and a dead dockerd, which is
		// exactly the state that took nxt-sw-3-w out of the cluster for hours.
		if watchdogDockerSocket != "" && nodePolicyArmsDocker(watchdogPolicyFile) {
			dockerState := filepath.Join(filepath.Dir(statePath), dockerWatchStateFile)
			_, derr := watchDockerOnce(ctx, dockerState, dockerWatchDeps{
				Probe:       func(ctx context.Context) error { return probeDockerSocket(ctx, watchdogDockerSocket) },
				UnitActive:  func(ctx context.Context) (bool, error) { return unitActiveNow(ctx, "docker") },
				Restart:     func(ctx context.Context) error { return restartUnit(ctx, "docker") },
				ResetFailed: func(ctx context.Context) error { return resetFailedUnit(ctx, "docker") },
				Out:         cmd.OutOrStdout(),
			})
			if derr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "docker-watch: %v\n", derr)
			}
		}
		if err != nil {
			return err
		}
		// A verdict is not an error: "the unit is not active" is a correct
		// decision, and a timer that exits non-zero on it would put a failed
		// unit in the journal every minute for a state it is right about.
		_ = verdict
		return nil
	},
}

func init() {
	healthWatchCmd.Flags().StringVar(&watchdogStatePath, "state-file", "",
		"path to the watchdog state file (default <data_dir>/"+watchdogStateFile+")")
	healthWatchCmd.Flags().StringVar(&watchdogProbeURL, "probe-url", defaultWatchdogProbeURL,
		"URL probed with a "+WatchdogProbeTimeout.String()+" timeout")
	healthWatchCmd.Flags().StringVar(&watchdogPolicyFile, "policy-file", "",
		"node policy document written by the leader's decision; without it the docker "+
			"check stays disarmed")
	healthWatchCmd.Flags().StringVar(&watchdogDockerSocket, "docker-socket", "",
		"also watch this docker socket and restart docker.service when it stops answering "+
			"(empty = disabled; the daemon cannot restart dockerd itself)")
	healthWatchCmd.Flags().StringVar(&watchdogUnit, "unit", "pmcluster",
		"systemd unit to probe and restart")
}

// hostLoopbackURL maps a configured listen address to the loopback URL the
// watchdog should probe. Kept next to the probe so a future listen-address
// change cannot leave the timer hammering an address nothing binds.
func hostLoopbackURL(listenAddr string) string {
	addr := strings.TrimSpace(listenAddr)
	if addr == "" {
		return defaultWatchdogProbeURL
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return defaultWatchdogProbeURL
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/health"
}
