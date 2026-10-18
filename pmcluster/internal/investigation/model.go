package investigation

import (
	"context"
	"fmt"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

// Hypothesis separates a possible cause from its cited observed evidence.
type Hypothesis struct {
	Cause      string   `json:"cause"`
	Confidence string   `json:"confidence"`
	Evidence   []string `json:"evidence"`
}

// Verdict is advisory output only; no model action is executable.
type Verdict struct {
	Summary    string       `json:"summary"`
	Evidence   []string     `json:"evidence"`
	Hypotheses []Hypothesis `json:"hypotheses"`
	Actions    []string     `json:"actions"`
}

// Validate rejects fabricated citation IDs and unbounded model output.
func (v Verdict) Validate(in Input) error {
	if strings.TrimSpace(v.Summary) == "" || len(v.Summary) > 8000 || len(v.Hypotheses) > 5 || len(v.Actions) > 8 || len(v.Evidence) == 0 {
		return fmt.Errorf("invalid verdict")
	}
	ids := map[string]bool{}
	for _, e := range in.Evidence {
		ids[e.ID] = true
	}
	check := func(refs []string) bool {
		if len(refs) == 0 || len(refs) > 40 {
			return false
		}
		for _, id := range refs {
			if !ids[id] {
				return false
			}
		}
		return true
	}
	if !check(v.Evidence) {
		return fmt.Errorf("invalid evidence citations")
	}
	for _, h := range v.Hypotheses {
		if len(h.Cause) == 0 || len(h.Cause) > 4000 || !check(h.Evidence) {
			return fmt.Errorf("invalid hypothesis")
		}
		if h.Confidence != "low" && h.Confidence != "medium" && h.Confidence != "high" {
			return fmt.Errorf("invalid confidence")
		}
	}
	for _, a := range v.Actions {
		if len(a) == 0 || len(a) > 2000 {
			return fmt.Errorf("invalid action")
		}
	}
	return nil
}

// Redact applies output redaction without changing validated evidence IDs.
func (v Verdict) Redact(r observability.Redactor) Verdict {
	v.Summary = r.Text(v.Summary)
	for i := range v.Hypotheses {
		v.Hypotheses[i].Cause = r.Text(v.Hypotheses[i].Cause)
	}
	for i := range v.Actions {
		v.Actions[i] = r.Text(v.Actions[i])
	}
	return v
}

// Provider executes exactly one bounded request without automatic retries.
type Provider interface {
	Run(context.Context, Policy, string, Input) (Verdict, *int64, error)
}
