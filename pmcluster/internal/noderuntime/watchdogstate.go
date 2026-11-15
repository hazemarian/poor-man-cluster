package noderuntime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WatchdogStateFile lives under the daemon's data dir, NOT in /run: the failure
// count has to survive the restart it triggers, or every restart would reset the
// count to zero and the watchdog would restart once a minute instead of once per
// rate limit.
const WatchdogStateFile = "watchdog.json"

// WatchdogState is the persisted evidence, one small JSON file.
type WatchdogState struct {
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

// loadWatchdogState reads the state file. A missing or corrupt file is an empty
// state, never an error: the watchdog must still work on a node whose data dir
// was just created, and a corrupt counter costs at most one extra minute of
// silence.
func LoadWatchdogState(path string) (WatchdogState, bool) {
	var st WatchdogState
	if strings.TrimSpace(path) == "" {
		return st, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return st, false
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return WatchdogState{}, false
	}
	return st, true
}

// saveWatchdogState writes the state 0600 via a rename, so a crash mid-write
// cannot leave a half-written counter that reads as zero.
func SaveWatchdogState(path string, st WatchdogState) error {
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
