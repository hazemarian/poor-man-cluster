// Package i18n holds the console's translation dictionaries and the Arabic
// pluralization rules.
//
// Rules implemented here follow the arabic-ui skill
// (https://github.com/JKc66/arabic-ui):
//   - widgets map to grammatical forms: buttons take المصدر (verbal noun),
//     placeholders take فعل الأمر (imperative), statuses take اسم الفاعل/المفعول
//   - completed actions use the internal passive (حُفظت التغييرات), never "تم + مصدر"
//   - possession uses the attached كاف الخطاب (حسابك), never "الخاص بك"
//   - capacity uses تعذّر / لم نتمكن من, never "فشل في"
//   - counts carry all six CLDR categories, including the dual with its
//     nominative/oblique inflection (see plural.go)
//   - dictionary completeness is enforced at test time, not by regex
//   - values carry no markup (templates escape once), identifiers and raw
//     upstream payloads are never translated, and value / unknown (the API did
//     not answer) / empty (the API answered with nothing) stay distinct
package i18n

import (
	"fmt"
	"strconv"
	"strings"
)

// Lang is a supported console language.
type Lang string

const (
	EN Lang = "en"
	AR Lang = "ar"
)

// Default is used when the request carries no usable preference.
const Default = EN

// Parse resolves a language tag ("ar", "ar-EG", "ar_SA") to a supported Lang.
func Parse(tag string) Lang {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if tag == "" {
		return Default
	}
	if strings.HasPrefix(tag, "ar") {
		return AR
	}
	return EN
}

// Supported lists the languages the switch offers, in display order.
func Supported() []Lang { return []Lang{EN, AR} }

// Localizer renders a single language. It is created per request and is safe
// for concurrent reads (dictionaries are written only from init()).
type Localizer struct {
	lang Lang
}

// New returns a localizer for lang, falling back to Default for unknown tags.
func New(lang Lang) *Localizer {
	switch lang {
	case EN, AR:
		return &Localizer{lang: lang}
	default:
		return &Localizer{lang: Default}
	}
}

// Lang returns the resolved language tag ("en" | "ar").
func (l *Localizer) Lang() string { return string(l.lang) }

// Dir returns the text direction for the document: "rtl" for Arabic.
func (l *Localizer) Dir() string {
	if l.lang == AR {
		return "rtl"
	}
	return "ltr"
}

// IsRTL reports whether the language renders right-to-left.
func (l *Localizer) IsRTL() bool { return l.lang == AR }

// T translates key. Unknown keys return a visible marker rather than an empty
// string, so a missing translation fails loudly in review instead of silently
// shipping a blank label.
func (l *Localizer) T(key string) string {
	if v, ok := dicts[l.lang][key]; ok {
		return v
	}
	if v, ok := dicts[Default][key]; ok {
		return v
	}
	return "⟦" + key + "⟧"
}

// TN translates key and treats an empty translation as absent (used where a
// missing optional label should not echo a marker).
func (l *Localizer) TN(key string) string {
	v := l.T(key)
	if strings.HasPrefix(v, "⟦") {
		return ""
	}
	return v
}

// TF translates key and substitutes positional placeholders: {0}, {1}, … .
// Substitution happens before the template escapes the result, so interpolated
// values are escaped exactly once and can never inject markup.
func (l *Localizer) TF(key string, args ...any) string {
	s := l.T(key)
	for i, a := range args {
		s = strings.ReplaceAll(s, "{"+strconv.Itoa(i)+"}", fmt.Sprint(a))
	}
	return s
}

// Has reports whether the language defines key.
func (l *Localizer) Has(key string) bool {
	_, ok := dicts[l.lang][key]
	return ok
}

// Plural renders a counted phrase through the six-category rules. dualCase is
// the grammatical case for the dual form (nominative by default); pass Oblique
// when the phrase follows a preposition or is a direct object.
func (l *Localizer) Plural(count int, key string, dualCase ...Case) string {
	c := Nominative
	if len(dualCase) > 0 {
		c = dualCase[0]
	}
	if f, ok := pluralForms(l.lang, key); ok {
		return formatPlural(count, f, c, l.lang)
	}
	if f, ok := pluralForms(Default, key); ok {
		return formatPlural(count, f, c, Default)
	}
	return l.T(key)
}

// P is the template-friendly alias for Plural (templates read better with a
// single letter: {{.L.P 3 "stacks.count"}}).
func (l *Localizer) P(count int, key string, dualCase ...Case) string {
	return l.Plural(count, key, dualCase...)
}

// N formats an integer with locale grouping: Western digits in both languages,
// because values here are copyable identifiers (ports, counts, byte sizes) and
// must stay monospaced and paste-safe. The skill's Arabic-digit mode is opt-in
// and deliberately not enabled.
func (l *Localizer) N(v int64) string { return group(v) }

// NF formats a float with one decimal, grouped.
func (l *Localizer) NF(v float64, decimals int) string { return groupFloat(v, decimals) }

// dicts holds every translation. Part files (dict_en_*.go / dict_ar_*.go)
// register into these from init() so the dictionaries can be written in
// reviewable chunks.
var dicts = map[Lang]map[string]string{EN: {}, AR: {}}

var plurals = map[Lang]map[string]PluralForms{EN: {}, AR: {}}

// register merges a part-file's entries into a language's dictionary.
// Re-registering an existing key is a programming error; tests assert on it.
func register(l Lang, pairs map[string]string) {
	for k, v := range pairs {
		dicts[l][k] = v
	}
}

// registerPlural adds a counted phrase.
func registerPlural(l Lang, key string, f PluralForms) {
	if plurals[l] == nil {
		plurals[l] = map[string]PluralForms{}
	}
	plurals[l][key] = f
}

// Keys returns every key registered for a language (test helper).
func Keys(l Lang) []string {
	out := make([]string, 0, len(dicts[l]))
	for k := range dicts[l] {
		out = append(out, k)
	}
	return out
}

// PluralKeys returns every plural key registered for a language (test helper).
func PluralKeys(l Lang) []string {
	out := make([]string, 0, len(plurals[l]))
	for k := range plurals[l] {
		out = append(out, k)
	}
	return out
}
