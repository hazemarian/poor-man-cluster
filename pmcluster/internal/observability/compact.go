package observability

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

// Evidence is one log message template with an exact citation ID. Records that
// differ only in numbers, ids, addresses or timestamps share a template, so a
// line repeated a thousand times costs one item rather than a thousand.
type Evidence struct {
	ID       string `json:"id"`
	Service  string `json:"service"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Count    int    `json:"count"`
	First    string `json:"first,omitempty"`
	Last     string `json:"last,omitempty"`
	// Example is one real record, present only when the template replaced
	// values or the record spans several lines; otherwise Message is exact.
	Example string `json:"example,omitempty"`
}

// Stats tells the model how much was folded away or left out, so it does not
// mistake a short list for a quiet system.
type Stats struct {
	Records        int `json:"records"`
	Templates      int `json:"templates"`
	OmittedItems   int `json:"omitted_templates"`
	OmittedRecords int `json:"omitted_records"`
}

// Input is the complete bounded question and evidence sent to a provider.
type Input struct {
	Question  string     `json:"question"`
	Start     string     `json:"start"`
	End       string     `json:"end"`
	Truncated bool       `json:"truncated"`
	Stats     Stats      `json:"stats"`
	Evidence  []Evidence `json:"evidence"`
}

const (
	maxEvidence      = 40
	maxTemplateRunes = 300
	maxExampleRunes  = 500
	maxBodyRunes     = 4000
)

// Per-severity shares of the evidence list. Unused share rolls down to the
// next bucket, so a quiet cluster still fills the list with whatever exists.
var severityQuota = [3]int{24, 10, 6}

// Placeholder patterns, most specific first. Order matters: a timestamp must
// be matched before its digits are taken as numbers.
var templateRules = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z|[+-]\d{2}:?\d{2})?`), "<TS>"},
	{regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "<ID>"},
	{regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b`), "<IP>"},
	{regexp.MustCompile(`\b(?:0x)?[0-9a-fA-F]{8,}\b`), "<ID>"},
	{regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ms|µs|us|ns|s|m|h|kb|mb|gb|b)\b`), "<DUR>"},
	{regexp.MustCompile(`\b\d+(?:\.\d+)?\b`), "<NUM>"},
}

// Template replaces the values that vary between otherwise identical records.
func Template(line string) string {
	for _, rule := range templateRules {
		line = rule.re.ReplaceAllString(line, rule.with)
	}
	return line
}

func severityRank(s string) int {
	switch strings.ToUpper(s) {
	case "ERROR", "FATAL", "CRITICAL", "PANIC", "ERR":
		return 0
	case "WARN", "WARNING":
		return 1
	default:
		return 2
	}
}

var levelHints = []struct {
	re *regexp.Regexp
}{
	{regexp.MustCompile(`(?i)\b(?:level|lvl|severity)["']?\s*[=:]\s*"?(trace|debug|info|notice|warn|warning|error|fatal|critical)\b`)},
	{regexp.MustCompile(`(?i)\t(debug|info|warn|warning|error|fatal)\t`)},
	{regexp.MustCompile(`(?i)^[\[(]?(debug|info|warn|warning|error|fatal|critical)[\])]?[ :]`)},
	{regexp.MustCompile(`^([IWEF])\d{4} \d`)},
	{regexp.MustCompile(`(?i)\][ ]+(log|notice|info|statement|debug\d?|warning|error|fatal|panic):`)},
}

// inferSeverity corrects the stream-derived level. Container output carries no
// level of its own, so the collector labels stderr ERROR; many programs log
// routine lines to stderr with their own level in the text. Only a clearly
// marked level in the first line overrides the label, and only for ERROR or
// unlabeled records. An explicit level from the application is never changed.
func inferSeverity(sev, firstLine string) string {
	if sev != "" && sev != "ERROR" && sev != "UNSPECIFIED" {
		return sev
	}
	head, _ := runesCap(firstLine, 200)
	for _, h := range levelHints {
		m := h.re.FindStringSubmatch(head)
		if m == nil {
			continue
		}
		level := strings.ToUpper(m[1])
		if strings.HasPrefix(level, "DEBUG") {
			level = "DEBUG"
		}
		switch level {
		case "LOG", "STATEMENT":
			return "INFO"
		case "I":
			return "INFO"
		case "W":
			return "WARN"
		case "E", "F":
			return "ERROR"
		case "TRACE":
			return "DEBUG"
		case "NOTICE":
			return "INFO"
		case "WARNING":
			return "WARN"
		case "CRITICAL":
			return "FATAL"
		default:
			return strings.ToUpper(m[1])
		}
	}
	return sev
}

func runesCap(s string, n int) (string, bool) {
	r := []rune(s)
	if len(r) <= n {
		return s, false
	}
	return string(r[:n]) + "…", true
}

// Compact folds records into message templates, ranks them so errors come
// first, and keeps the list within a fixed size.
func Compact(question, start, end string, rows []Row, truncated bool) Input {
	in := Input{Question: question, Start: start, End: end, Truncated: truncated}
	groups := map[string]*Evidence{}
	var order []string
	for _, row := range rows {
		log := ProjectLog(row)
		if strings.TrimSpace(log.Body) == "" {
			continue
		}
		in.Stats.Records++
		body, cut := runesCap(strings.TrimSpace(log.Body), maxBodyRunes)
		if cut {
			in.Truncated = true
		}
		first, rest, multi := strings.Cut(body, "\n")
		multi = multi && strings.TrimSpace(rest) != ""
		tmpl, _ := runesCap(strings.TrimSpace(Template(first)), maxTemplateRunes)
		sev := inferSeverity(strings.ToUpper(log.Severity), strings.TrimSpace(first))
		key := log.Service + "\x00" + sev + "\x00" + tmpl
		if multi {
			key += "\x00+"
		}
		g := groups[key]
		if g == nil {
			g = &Evidence{Service: log.Service, Severity: sev, Message: tmpl}
			if multi || tmpl != strings.TrimSpace(first) {
				g.Example, _ = runesCap(body, maxExampleRunes)
			}
			sum := sha256.Sum256([]byte(start + "\x00" + end + "\x00" + key))
			g.ID = "log:" + hex.EncodeToString(sum[:12])
			groups[key] = g
			order = append(order, key)
		}
		g.Count++
		if ts := log.Timestamp; ts != "" {
			if g.First == "" || ts < g.First {
				g.First = ts
			}
			if ts > g.Last {
				g.Last = ts
			}
		}
	}
	in.Stats.Templates = len(groups)

	var buckets [3][]*Evidence
	for _, key := range order {
		g := groups[key]
		buckets[severityRank(g.Severity)] = append(buckets[severityRank(g.Severity)], g)
	}
	for rank, b := range buckets {
		sort.SliceStable(b, func(i, j int) bool {
			if b[i].Count != b[j].Count {
				// Repeated errors and warnings are the signal; repeated info is
				// usually a heartbeat, so unusual info lines come first.
				if rank < 2 {
					return b[i].Count > b[j].Count
				}
				return b[i].Count < b[j].Count
			}
			return b[i].ID < b[j].ID
		})
	}
	taken := [3]int{}
	budget := maxEvidence
	for pass := 0; pass < 2 && budget > 0; pass++ {
		for rank := range buckets {
			limit := severityQuota[rank]
			if pass == 1 {
				limit = len(buckets[rank])
			}
			for taken[rank] < len(buckets[rank]) && taken[rank] < limit && budget > 0 {
				in.Evidence = append(in.Evidence, *buckets[rank][taken[rank]])
				taken[rank]++
				budget--
			}
		}
	}
	for rank, b := range buckets {
		for _, g := range b[taken[rank]:] {
			in.Stats.OmittedItems++
			in.Stats.OmittedRecords += g.Count
		}
	}
	if in.Stats.OmittedItems > 0 {
		in.Truncated = true
	}
	sort.SliceStable(in.Evidence, func(i, j int) bool {
		return severityRank(in.Evidence[i].Severity) < severityRank(in.Evidence[j].Severity)
	})
	return in
}
