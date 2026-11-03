package backups

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Retention keeps a short dense window and a long sparse one.
//
// Hourly artifacts are what an operator actually reaches for ("restore the state
// from before the bad deploy this morning"), so they are kept for a couple of
// days in full. Beyond that the value is "a copy from a while ago", which does
// not need 24 copies per day: one per calendar day is enough, and it is what
// makes a month of history fit in the same disk.
//
// The two rules are deliberately separate knobs, because they fail differently:
// KeepHours controls how far back you can go hour by hour, KeepDays how far back
// at all. Both are counted from `now`, not from the newest artifact — a node that
// stopped taking backups yesterday must not have its history silently judged
// young and deleted a month from now.
type Retention struct {
	// KeepHours is the dense window: every artifact newer than this is kept.
	KeepHours int
	// KeepDays is the sparse window: beyond KeepHours, one artifact per calendar
	// day is kept until this age.
	KeepDays int
	// KeepLast is a floor, not a policy: the newest N artifacts of every stack
	// are never deleted, whatever the clocks say. Losing the only copy of a stack
	// because its timestamps looked old is not a retention decision.
	KeepLast int
	// IncludeLegacy also prunes whole-node archives (offen's output, no stack in
	// the name). Off by default: on a fleet mid-cutover those are the ONLY backups
	// of the nodes that have not moved yet.
	IncludeLegacy bool
}

// DefaultRetention is 48 hours dense, 15 days sparse — the same 15 days offen was
// configured with, so the cutover does not shorten anyone's history.
func DefaultRetention() Retention {
	return Retention{KeepHours: 48, KeepDays: 15, KeepLast: 2}
}

// PruneResult reports what a prune did, so a log line can be specific.
type PruneResult struct {
	Kept       int      `json:"kept"`
	Deleted    []string `json:"deleted,omitempty"`
	FreedBytes int64    `json:"freed_bytes"`
	Failed     []string `json:"failed,omitempty"`
	SkippedOld int      `json:"skipped_legacy,omitempty"`
	DryRun     bool     `json:"dry_run,omitempty"`
}

// PruneArtifacts deletes artifacts in dir that fall outside the retention policy.
//
// It works per stack: each stack's history is judged on its own, so one stack
// that has been quiet for a month cannot cause another stack's artifacts to be
// deleted, and a stack that only appears lately is not measured against a
// neighbour's long history.
func PruneArtifacts(dir string, policy Retention, now time.Time, dryRun bool) (PruneResult, error) {
	res := PruneResult{DryRun: dryRun}
	if policy.KeepLast < 1 {
		policy.KeepLast = 1
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return res, nil
		}
		return res, fmt.Errorf("read %s: %w", dir, err)
	}

	byStack := map[string][]Artifact{}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		art, ok := ParseArtifact(e.Name())
		if !ok {
			continue
		}
		if art.Stack == "" && !policy.IncludeLegacy {
			res.SkippedOld++
			continue
		}
		// Grouped by node AND stack: legacy whole-node archives have no stack, and
		// grouping them under "" would let one node's presence keep another node's
		// only archive alive (or vice versa).
		byStack[art.Node+"/"+art.Stack] = append(byStack[art.Node+"/"+art.Stack], art)
	}

	for _, arts := range byStack {
		sort.Slice(arts, func(i, j int) bool { return arts[i].At.After(arts[j].At) })
		newestPerDay := map[string]Artifact{}
		for _, a := range arts {
			day := a.At.Format("2006-01-02")
			if _, seen := newestPerDay[day]; !seen {
				newestPerDay[day] = a
			}
		}
		for i, a := range arts {
			if i < policy.KeepLast {
				res.Kept++
				continue
			}
			if !shouldDelete(a.At, policy, now, newestPerDay[a.At.Format("2006-01-02")].Name == a.Name) {
				res.Kept++
				continue
			}
			// A dry run must not call the delete path at all: doing the work and
			// reporting it as hypothetical is how "dry run" deletes data.
			if dryRun {
				if fi, statErr := os.Stat(filepath.Join(dir, a.Name)); statErr == nil {
					res.FreedBytes += fi.Size()
				}
				res.Deleted = append(res.Deleted, a.Name+" (would delete)")
				continue
			}
			size, _, err := removeArtifact(dir, a.Name)
			if err != nil {
				res.Failed = append(res.Failed, a.Name+": "+err.Error())
				res.Kept++
				continue
			}
			res.Deleted = append(res.Deleted, a.Name)
			res.FreedBytes += size
		}
	}
	return res, nil
}

// shouldDelete decides one artifact. The empty day key means the artifact's own
// day is represented by itself (so it is the one copy that day worth keeping).
func shouldDelete(at time.Time, policy Retention, now time.Time, isNewestOfDay bool) bool {
	age := now.Sub(at)
	if age < time.Duration(policy.KeepHours)*time.Hour {
		return false
	}
	if age > time.Duration(policy.KeepDays)*24*time.Hour {
		return true
	}
	// Inside the sparse window: keep one per day, drop the rest.
	return !isNewestOfDay
}

// removeArtifact deletes an artifact and its sidecar, returning the bytes freed.
// A sidecar deleted without its archive (or the reverse) leaves a record that
// points at a file which is not there, which is worse than either alone.
func removeArtifact(dir, name string) (int64, bool, error) {
	var freed int64
	main := filepath.Join(dir, name)
	if fi, err := os.Stat(main); err == nil {
		freed += fi.Size()
	}
	if err := os.Remove(main); err != nil && !os.IsNotExist(err) {
		return 0, false, err
	}
	if err := os.Remove(main + ".json"); err != nil && !os.IsNotExist(err) {
		return freed, true, err
	}
	return freed, true, nil
}
