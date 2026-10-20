package observability

import (
	"regexp"
	"strings"
	"unicode"
)

var terminalEscape = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\))`)

// CleanTerminalText removes terminal control sequences before evidence is
// printed in a table or log stream. Newlines and tabs are retained for log
// bodies; callers rendering one table cell should use FlattenTerminalText.
func CleanTerminalText(value string) string {
	value = terminalEscape.ReplaceAllString(value, "")
	var b strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// FlattenTerminalText makes a backend value safe for a single tabular cell.
func FlattenTerminalText(value string) string {
	value = CleanTerminalText(value)
	return strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(value)
}
