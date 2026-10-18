package observability

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	redactJWT    = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)
	redactAWS    = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
	redactBearer = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	redactBasic  = regexp.MustCompile(`(?i)\bBasic\s+[A-Za-z0-9+/=]{8,}`)
	redactAuth   = regexp.MustCompile(`(?i)(\bAuthorization\s*[:=]\s*)(Bearer|Basic)\s+[^\s,;]+`)
	redactEmail  = regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)
	redactPhone  = regexp.MustCompile(`\+?[0-9][0-9 .()_-]{7,}[0-9]`)
	// A bare calendar date matches the phone shape but is not sensitive, and
	// redacting it makes every timestamp in a log line unreadable.
	isoDate        = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	redactIP       = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}(?::[0-9]{1,5})?\b`)
	redactURLCreds = regexp.MustCompile(`(?i)(://[^/:\s]+):([^@\s]+)@`)
	redactAssign   = regexp.MustCompile(`(?i)(["']?\b(?:password|passwd|secret|token|api[_-]?key)\b["']?\s*[:=]\s*)("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s,;}]+)`)
)

// Redactor masks common credential and direct-identifier shapes in evidence.
// Exact values should include every server-side secret available to the
// caller.  Values are replaced longest-first so a short token cannot reveal a
// suffix of a longer credential.
type Redactor struct {
	exact []string
}

// NewRedactor constructs a deterministic redactor, dropping empty values and
// duplicate entries.  The values are never returned or logged.
func NewRedactor(exact ...string) Redactor {
	seen := make(map[string]struct{}, len(exact))
	out := make([]string, 0, len(exact))
	for _, value := range exact {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if len(out[j]) > len(out[i]) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return Redactor{exact: out}
}

// Text masks a string while retaining protocol labels and field names.
func (r Redactor) Text(value string) string {
	result := value
	for _, secret := range r.exact {
		result = strings.ReplaceAll(result, secret, "<REDACTED>")
		// A row can arrive after a JSON round trip where quotes and backslashes
		// are escaped.  Replace that representation too, without ever exposing
		// the secret while constructing the replacement.
		quoted := strconv.Quote(secret)
		if len(quoted) >= 2 {
			result = strings.ReplaceAll(result, quoted[1:len(quoted)-1], "<REDACTED>")
		}
	}
	result = redactURLCreds.ReplaceAllString(result, `${1}:<REDACTED>@`)
	result = redactJWT.ReplaceAllString(result, "<JWT>")
	result = redactAWS.ReplaceAllString(result, "<AWS_KEY>")
	result = redactAuth.ReplaceAllString(result, `${1}${2} <REDACTED>`)
	result = redactBearer.ReplaceAllString(result, "Bearer <REDACTED>")
	result = redactBasic.ReplaceAllString(result, "Basic <REDACTED>")
	result = redactAssign.ReplaceAllStringFunc(result, func(match string) string {
		parts := redactAssign.FindStringSubmatch(match)
		if len(parts) != 3 {
			return "<REDACTED>"
		}
		value := parts[2]
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			return parts[1] + value[:1] + "<REDACTED>" + value[len(value)-1:]
		}
		return parts[1] + "<REDACTED>"
	})
	result = redactEmail.ReplaceAllString(result, "<EMAIL>")
	result = redactIP.ReplaceAllString(result, `<ADDR>`)
	result = redactPhone.ReplaceAllStringFunc(result, func(match string) string {
		if isoDate.MatchString(match) {
			return match
		}
		return "<PHONE>"
	})
	return result
}

// Value recursively redacts arbitrary decoded JSON values.  It is safe to
// pass through a template because strings remain ordinary strings and no
// template.HTML is produced here.
func (r Redactor) Value(value any) any {
	switch v := value.(type) {
	case string:
		return r.Text(v)
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = r.Value(v[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if key == "p_timestamp" || key == "timestamp" || key == "time" {
				if text, ok := item.(string); ok {
					if validEvidenceTimestamp(text) && !r.containsExact(text) {
						out[key] = text
						continue
					}
				}
			}
			if sensitiveField(key) {
				out[r.Text(key)] = "<REDACTED>"
				continue
			}
			out[r.Text(key)] = r.Value(item)
		}
		return out
	default:
		return value
	}
}

func (r Redactor) containsExact(value string) bool {
	for _, secret := range r.exact {
		if strings.Contains(value, secret) {
			return true
		}
	}
	return false
}

func sensitiveField(key string) bool {
	switch strings.ToLower(strings.Trim(key, " \t\"'")) {
	case "password", "passwd", "secret", "token", "api_key", "api-key", "apikey", "authorization":
		return true
	default:
		return false
	}
}

// Rows returns a redacted copy of records without mutating the adapter's
// decoded values.
func (r Redactor) Rows(rows []Row) []Row {
	out := make([]Row, len(rows))
	for i, row := range rows {
		out[i] = Row(r.Value(map[string]any(row)).(map[string]any))
	}
	return out
}

// SafeError preserves only a short, redacted diagnostic suitable for logs or a
// folded operator disclosure.  It intentionally does not include HTTP bodies
// or credentials from an adapter request.
func (r Redactor) SafeError(err error) string {
	if err == nil {
		return ""
	}
	const max = 512
	text := r.Text(fmt.Sprint(err))
	if len(text) > max {
		text = text[:max] + "…"
	}
	return text
}

// WithExact extends the redactor with additional request-specific secrets.
func (r Redactor) WithExact(values ...string) Redactor {
	return NewRedactor(append(append([]string(nil), r.exact...), values...)...)
}

// Parseable serializes UTC Arrow timestamps without a timezone suffix.
// Preserve only valid typed timestamp fields, never arbitrary message text.
func validEvidenceTimestamp(value string) bool {
	if _, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return true
	}
	_, err := time.Parse("2006-01-02T15:04:05.999999999", value)
	return err == nil
}
