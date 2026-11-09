package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// ApplyRecord is the durable answer to "did the platform actually change, and
// who changed it?".
//
// The daemon's leader-only reconcile loop runs the same update pipeline with its
// output discarded. So an upgrade's template refresh and stack re-deploy can be
// applied BY THE LOOP before an operator ever runs `cluster update` — and the
// operator's run then reports "no stacks re-deployed and no configs replaced",
// which reads as "the upgrade did nothing". The apply is only invisible because
// nothing wrote it down.
//
// So: every run that moves something records it here, and a run that moves
// nothing reports the last one that did. "Nothing to redeploy" then means what
// it says — nothing changed in this run — instead of contradicting the live
// Swarm.
type ApplyRecord struct {
	At      time.Time `json:"at"`
	Version string    `json:"version"`
	Source  string    `json:"source"`
	Stacks  []string  `json:"stacks,omitempty"`
	Configs []string  `json:"configs,omitempty"`

	// Warnings is what the run could not do, kept because the run that emits
	// them is often unattended (the daemon's reconcile loop) and its stdout
	// only reaches journald. Without this, a capability the platform quietly
	// lost — a swarm mirror it could not rebuild, a node it could not label —
	// was invisible to everyone who was not watching a terminal at the time.
	// Recording them also means a warning-only run is a run worth recording.
	Warnings []string `json:"warnings,omitempty"`
}

// lastApplySettingKey lives in cluster_settings: no migration, and it is
// operator-visible state, not a secret.
const lastApplySettingKey = "last_platform_apply"

// Empty reports whether the record describes a run that changed nothing AND
// degraded nothing.
//
// Warnings count. A run that deployed no stacks but could not rebuild four
// secret mirrors is the single record an operator most needs to see — it changed
// nothing, so nothing else about it leaves a trace, and gating the record on
// "did it change something" is how those warnings stayed invisible.
func (r ApplyRecord) Empty() bool {
	return len(r.Stacks) == 0 && len(r.Configs) == 0 && len(r.Warnings) == 0
}

// Line renders the record the way both the daemon log and the CLI summary show
// it, so the same sentence appears whichever one the operator reads.
func (r ApplyRecord) Line() string {
	stacks, configs := "no stacks", "no configs"
	if len(r.Stacks) > 0 {
		stacks = fmt.Sprintf("%d stack(s) %v", len(r.Stacks), r.Stacks)
	}
	if len(r.Configs) > 0 {
		configs = fmt.Sprintf("%d rendered config(s) %v", len(r.Configs), r.Configs)
	}
	line := fmt.Sprintf("last platform apply: %s by %s (%s) — re-deployed %s, replaced %s",
		r.At.UTC().Format(time.RFC3339), r.Source, r.Version, stacks, configs)
	if n := len(r.Warnings); n > 0 {
		line += fmt.Sprintf(", %d warning(s): %s", n, r.Warnings[0])
	}
	return line
}

// LoadApplyRecord reads the most recent apply, or nil when none was ever
// recorded (a cluster that has not run the pipeline since this shipped).
func LoadApplyRecord(ctx context.Context, st *store.Store) *ApplyRecord {
	if st == nil {
		return nil
	}
	raw, err := st.Setting(ctx, lastApplySettingKey)
	if err != nil || raw == "" {
		return nil
	}
	var rec ApplyRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil
	}
	if rec.Empty() {
		return nil
	}
	return &rec
}

// SaveApplyRecord persists an apply. A failure here must never fail the update —
// the platform is already reconciled at this point, and the record is evidence,
// not state the pipeline depends on.
func SaveApplyRecord(ctx context.Context, st *store.Store, rec ApplyRecord) error {
	if st == nil || rec.Empty() {
		return nil
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return st.SetSetting(ctx, lastApplySettingKey, string(b))
}

// maxApplyWarnings bounds the record. An unattended loop that fails every pass
// on the same five repos would otherwise grow the row without limit; the record
// exists to be read, and the first failures are the ones worth reading.
const maxApplyWarnings = 20

// AppendApplyWarnings merges warnings raised OUTSIDE the platform apply
// pipeline into the last apply record.
//
// The runner lifecycle is a reconcile pass of its own (it deploys one-service
// stacks imperatively, not through the content-hash pipeline), but its failures
// have the same problem the pipeline's do: they happen in the daemon's journal,
// unattended, and the operator's next `cluster update` would report a clean run.
// Recording them here means `pmcluster doctor`'s "last apply" check names them.
//
// Warnings are deduplicated by string so a repo failing every pass does not
// append the same sentence sixty times an hour. A record created only for
// warnings is legitimate — ApplyRecord.Empty() counts warnings as content.
func AppendApplyWarnings(ctx context.Context, st *store.Store, source, version string, warnings []string) error {
	if st == nil || len(warnings) == 0 {
		return nil
	}
	rec := ApplyRecord{At: time.Now(), Source: source, Version: version}
	if prev := LoadApplyRecord(ctx, st); prev != nil {
		rec = *prev
	}
	seen := make(map[string]bool, len(rec.Warnings))
	for _, w := range rec.Warnings {
		seen[w] = true
	}
	changed := false
	for _, w := range warnings {
		if w == "" || seen[w] {
			continue
		}
		seen[w] = true
		rec.Warnings = append(rec.Warnings, w)
		changed = true
	}
	if !changed {
		return nil
	}
	if len(rec.Warnings) > maxApplyWarnings {
		rec.Warnings = rec.Warnings[:maxApplyWarnings]
	}
	// The record's timestamp is when it was last written: a warning-only run
	// is a state change as far as the operator is concerned.
	rec.At = time.Now()
	if source != "" {
		rec.Source = source
	}
	if version != "" {
		rec.Version = version
	}
	return SaveApplyRecord(ctx, st, rec)
}
