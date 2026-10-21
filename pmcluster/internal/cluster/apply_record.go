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
}

// lastApplySettingKey lives in cluster_settings: no migration, and it is
// operator-visible state, not a secret.
const lastApplySettingKey = "last_platform_apply"

// Empty reports whether the record describes a run that changed nothing.
func (r ApplyRecord) Empty() bool { return len(r.Stacks) == 0 && len(r.Configs) == 0 }

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
	return fmt.Sprintf("last platform apply: %s by %s (%s) — re-deployed %s, replaced %s",
		r.At.UTC().Format(time.RFC3339), r.Source, r.Version, stacks, configs)
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
