package runners

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// stateSettingKey is the durable bookkeeping for what this node registered.
// Deliberately a settings row rather than a table: it is small, it is not
// queried, and it is operator-visible state (not a secret) — the same call the
// platform apply record makes.
//
// It is keyed by the RUNNER NAME, not by the organisation. Each node owns exactly
// one name (pmc-<node>-org) and only ever tears down its own: a store shared by
// several nodes must not let one node de-register another node's runner.
//
// The container is discovered by name, so this state is only needed for the half a
// removed container cannot tell you: which runner NAME to de-register at GitHub
// after the feature is turned off while GitHub was unreachable, or after the daemon
// restarts between removing the container and deleting the registration.
const stateSettingKey = "github_runners_state"

// RunnerState is what this node knows about the runner it owns.
type RunnerState struct {
	// Name is the GitHub-visible runner name (pmc-<node>-org). It is the map key
	// too, duplicated here so a state file read by eye says which runner an entry
	// is about.
	Name string `json:"name"`
	// Org is the organisation the runner is registered in — de-registration needs
	// it, and it can differ from today's setting after an org change.
	Org string `json:"org"`
	// RegisteredAt is when the container was last started with a fresh
	// registration token. It drives the recycle cooldown: a container that is
	// still pulling its image must not be re-created every pass.
	RegisteredAt int64 `json:"registered_at,omitempty"`
	// Failures is the consecutive registration/start failure count, reported by
	// the doctor as "failed repeatedly" when it runs high.
	Failures  int    `json:"failures,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// State is the persisted bookkeeping: one entry per runner name this node owns,
// plus the operator holds.
//
// A leftover entry is not just a cache. It is the platform's only record that a
// registration may still exist at GitHub, which is why teardown keeps it until the
// de-registration is confirmed and the doctor reports it as a leftover when the
// feature is off.
type State struct {
	Runners map[string]RunnerState `json:"runners"`
	// Killed holds the runner names an operator stopped from the console, with the
	// unix time of the kill. A held runner is NOT re-registered — otherwise a Kill
	// button would be undone within one pass and Restart would mean nothing.
	// Restart, or removing the node from github_runner_nodes, drops the hold.
	Killed map[string]int64 `json:"killed,omitempty"`
}

func loadState(ctx context.Context, st *store.Store) State {
	s := State{Runners: map[string]RunnerState{}, Killed: map[string]int64{}}
	if st == nil {
		return s
	}
	raw, err := st.Setting(ctx, stateSettingKey)
	if err != nil || raw == "" {
		return s
	}
	var parsed State
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		// Corrupt bookkeeping is not worth failing a reconcile over: the container
		// is discoverable by name, and the worst case is a leftover registration
		// the doctor names.
		return s
	}
	if parsed.Runners != nil {
		s.Runners = parsed.Runners
	}
	if parsed.Killed != nil {
		s.Killed = parsed.Killed
	}
	return s
}

func saveState(ctx context.Context, st *store.Store, s State) error {
	if st == nil {
		return nil
	}
	if s.Runners == nil {
		s.Runners = map[string]RunnerState{}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return st.SetSetting(ctx, stateSettingKey, string(b))
}

// held reports whether an operator stopped this runner from the console.
func (s State) held(name string) bool {
	_, ok := s.Killed[name]
	return ok
}

// dropHold forgets a kill, so the reconcile may register the runner again.
func (s State) dropHold(name string) bool {
	if _, ok := s.Killed[name]; !ok {
		return false
	}
	delete(s.Killed, name)
	return true
}

// entry returns this node's bookkeeping for a runner name.
func (s State) entry(name string) (RunnerState, bool) {
	e, ok := s.Runners[name]
	return e, ok
}

// registeredAtTime returns the last registration time (zero when never).
func (r RunnerState) registeredAtTime() time.Time {
	if r.RegisteredAt <= 0 {
		return time.Time{}
	}
	return time.Unix(r.RegisteredAt, 0)
}
