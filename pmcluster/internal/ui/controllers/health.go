package controllers

import (
	"context"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

// healthModel is one failing or warning platform check.
type healthModel struct {
	Name   string
	Detail string
	// Failed is a fail; false is a warn. Both are shown, the banner tone follows the worst.
	Failed bool
}

// healthView is the banner state a screen needs: the rows, whether the daemon answered, and the
// tone ("bad" when any check failed, "warn" when only warnings remain — a template cannot call
// `if` as an argument, so the model carries the answer).
type healthView struct {
	Rows  []healthModel
	Known bool
	Tone  string
}

// loadHealth asks the daemon for its platform checks and keeps the ones that are not passing.
//
// Every screen that shows state a check measures should carry this: the overview had the banner
// and the backups screen did not, so "a storage node has never reported a backup" was one nav
// click away from the page about backups. `only` narrows it to a subject — the check's own name
// is the subject, and the daemon names them like "backup coverage", "unbacked data", "restore
// drill", "offsite".
func loadHealth(ctx context.Context, api *pmapi.Client, only ...string) healthView {
	checks, err := api.Doctor(ctx)
	if err != nil || checks == nil {
		return healthView{}
	}
	if len(only) == 0 {
		rows, known, tone := shapeHealth(checks)
		return healthView{Rows: rows, Known: known, Tone: tone}
	}
	var kept []pmapi.DoctorCheck
	for _, ch := range checks {
		if matchAny(ch.Name, only) {
			kept = append(kept, ch)
		}
	}
	rows, known, tone := shapeHealth(kept)
	return healthView{Rows: rows, Known: known, Tone: tone}
}

// shapeHealth keeps the checks worth printing — fail and warn — and derives the banner tone.
// "ok" is not news, and "skip" is a check declining to answer on this node (the run history is
// not mirrored, for instance), so reporting either as a finding would be a lie in both directions.
func shapeHealth(checks []pmapi.DoctorCheck) ([]healthModel, bool, string) {
	rows := make([]healthModel, 0, len(checks))
	tone := ""
	for _, ch := range checks {
		if ch.Status != "fail" && ch.Status != "warn" {
			continue
		}
		rows = append(rows, healthModel{Name: ch.Name, Detail: ch.Detail, Failed: ch.Status == "fail"})
		if ch.Status == "fail" {
			tone = "bad"
		}
	}
	if len(rows) > 0 && tone == "" {
		tone = "warn"
	}
	return rows, true, tone
}

// matchAny reports whether a check name contains any of the subjects.
func matchAny(name string, subjects []string) bool {
	for _, s := range subjects {
		if s != "" && containsFold(name, s) {
			return true
		}
	}
	return false
}

// containsFold is strings.Contains, case-insensitively, without the allocation of lowering both
// when the lengths cannot match.
func containsFold(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if equalFold(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
