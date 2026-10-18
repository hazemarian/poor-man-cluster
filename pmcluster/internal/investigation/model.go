package investigation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

// Evidence is a compact, redacted log template with an exact citation ID.
type Evidence struct {
	ID       string `json:"id"`
	Service  string `json:"service"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Count    int    `json:"count"`
}

// Input is the complete bounded question and evidence sent to a provider.
type Input struct {
	Question  string     `json:"question"`
	Start     string     `json:"start"`
	End       string     `json:"end"`
	Truncated bool       `json:"truncated"`
	Evidence  []Evidence `json:"evidence"`
}

// Compact groups identical redacted records and prioritizes error evidence.
func Compact(question, start, end string, rows []observability.Row, truncated bool) Input {
	in := Input{Question: question, Start: start, End: end, Truncated: truncated}
	groups := map[string]*Evidence{}
	for _, row := range rows {
		log := observability.ProjectLog(row)
		if log.Body == "" {
			continue
		}
		body := []rune(log.Body)
		if len(body) > 500 {
			body = body[:500]
			in.Truncated = true
		}
		key := log.Service + "\x00" + strings.ToUpper(log.Severity) + "\x00" + string(body)
		if e := groups[key]; e != nil {
			e.Count++
			continue
		}
		sum := sha256.Sum256([]byte(start + "\x00" + end + "\x00" + key))
		groups[key] = &Evidence{ID: "log:" + hex.EncodeToString(sum[:12]), Service: log.Service, Severity: strings.ToUpper(log.Severity), Message: string(body), Count: 1}
	}
	for _, item := range groups {
		in.Evidence = append(in.Evidence, *item)
	}
	priority := func(s string) int {
		switch s {
		case "ERROR", "FATAL":
			return 0
		case "WARN", "WARNING":
			return 1
		default:
			return 2
		}
	}
	sort.Slice(in.Evidence, func(i, j int) bool {
		a, b := in.Evidence[i], in.Evidence[j]
		if priority(a.Severity) != priority(b.Severity) {
			return priority(a.Severity) < priority(b.Severity)
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.ID < b.ID
	})
	if len(in.Evidence) > 40 {
		in.Evidence = in.Evidence[:40]
		in.Truncated = true
	}
	return in
}

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
