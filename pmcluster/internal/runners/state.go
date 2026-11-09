package runners

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// stateSettingKey is the durable bookkeeping for what the platform registered.
// Deliberately a settings row rather than a table: it is small, it is not
// queried, and it is operator-visible state (not a secret) — the same call the
// platform apply record makes.
//
// The containers themselves are discovered from their service labels, so this
// state is only needed for the half a removed container cannot tell you: which
// runner NAME to de-register on GitHub after a repo leaves the configured list
// (or the daemon restarts between the two steps).
const stateSettingKey = "github_runners_state"

// RepoState is what the platform knows about one repo's runner.
type RepoState struct {
	// Service is the one-service stack name (StackName(repo)).
	Service string `json:"service"`
	// Name is the GitHub-visible runner name, so de-registration does not
	// have to guess the node half of pmc-<node>-<repo-short>.
	Name string `json:"name"`
	// RegisteredAt is when the container was last (re)created with a fresh
	// registration token. It drives the recycle cooldown: a container that is
	// still pulling its image is at 0 replicas and must not be re-minted every
	// pass.
	RegisteredAt int64 `json:"registered_at,omitempty"`
	// Failures is the consecutive registration/deploy failure count, reported
	// by the doctor as "failed repeatedly" when it runs high.
	Failures  int    `json:"failures,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// State is the persisted bookkeeping map, keyed by repo.
type State struct {
	Repos map[string]RepoState `json:"repos"`
}

func loadState(ctx context.Context, st *store.Store) State {
	s := State{Repos: map[string]RepoState{}}
	if st == nil {
		return s
	}
	raw, err := st.Setting(ctx, stateSettingKey)
	if err != nil || raw == "" {
		return s
	}
	var parsed State
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		// Corrupt bookkeeping is not worth failing a reconcile over: the
		// containers are still discoverable by label, and the worst case is a
		// leftover registration the doctor names.
		return s
	}
	if parsed.Repos != nil {
		s.Repos = parsed.Repos
	}
	return s
}

func saveState(ctx context.Context, st *store.Store, s State) error {
	if st == nil {
		return nil
	}
	if s.Repos == nil {
		s.Repos = map[string]RepoState{}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return st.SetSetting(ctx, stateSettingKey, string(b))
}

// registeredAtTime returns the last registration time (zero when never).
func (r RepoState) registeredAtTime() time.Time {
	if r.RegisteredAt <= 0 {
		return time.Time{}
	}
	return time.Unix(r.RegisteredAt, 0)
}
