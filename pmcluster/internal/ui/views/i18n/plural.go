package i18n

import (
	"fmt"
	"strconv"
	"strings"
)

// Case is the grammatical case of the Arabic dual (المثنى): nominative
// (مرفوع بالألف: تطبيقان) or oblique (منصوب/مجرور بالياء: تطبيقين).
type Case string

const (
	// Nominative — subject, predicate, standalone counter ("تطبيقان متبقيان").
	Nominative Case = "nominative"
	// Oblique — after a preposition or as a direct object ("قبل دقيقتين").
	Oblique Case = "oblique"
)

// PluralForms holds the six CLDR categories for one counted noun.
//
// Arabic example (backup, "نسخة احتياطية"):
//
//	Zero: لا توجد نسخ احتياطية
//	One:  نسخة احتياطية واحدة
//	Two:  نسختان / نسختين
//	Few:  {n} نسخ احتياطية        (3–10)
//	Many: {n} نسخةً احتياطية      (11–99)
//	Other:{n} نسخة احتياطية       (100+)
//
// English example:
//
//	One:  {n} backup
//	Other:{n} backups
type PluralForms struct {
	Zero string
	One  string
	// TwoNom / TwoObl are the nominative and oblique dual forms. When TwoObl
	// is empty the nominative form is used for both cases.
	TwoNom string
	TwoObl string
	Few    string
	Many   string
	Other  string
}

// category selects the CLDR plural category for a count in a language.
func category(l Lang, n int) string {
	if l == EN {
		if n == 1 {
			return "one"
		}
		return "other"
	}
	// Arabic: 0 zero · 1 one · 2 two · n%100 3-10 few · n%100 11-99 many · else other
	switch n {
	case 0:
		return "zero"
	case 1:
		return "one"
	case 2:
		return "two"
	}
	mod := n % 100
	switch {
	case mod >= 3 && mod <= 10:
		return "few"
	case mod >= 11 && mod <= 99:
		return "many"
	default:
		return "other"
	}
}

// pick resolves the template string for a category, degrading gracefully so a
// partially filled PluralForms never renders empty text.
func (f PluralForms) pick(cat string, c Case) string {
	switch cat {
	case "zero":
		if f.Zero != "" {
			return f.Zero
		}
		if f.Other != "" {
			return f.Other
		}
	case "one":
		if f.One != "" {
			return f.One
		}
		if f.Other != "" {
			return f.Other
		}
	case "two":
		if c == Oblique && f.TwoObl != "" {
			return f.TwoObl
		}
		if f.TwoNom != "" {
			return f.TwoNom
		}
		if f.TwoObl != "" {
			return f.TwoObl
		}
		if f.Other != "" {
			return f.Other
		}
	case "few":
		if f.Few != "" {
			return f.Few
		}
		if f.Other != "" {
			return f.Other
		}
	case "many":
		if f.Many != "" {
			return f.Many
		}
		if f.Other != "" {
			return f.Other
		}
	}
	return f.Other
}

// formatPlural renders count through the language's plural rules.
func formatPlural(count int, f PluralForms, c Case, l Lang) string {
	cat := category(l, count)
	tpl := f.pick(cat, c)
	// A form without the {n} placeholder (Arabic zero, dual) stands alone —
	// the numeral is grammatically wrong to append there.
	if !strings.Contains(tpl, "{n}") {
		return tpl
	}
	return strings.ReplaceAll(tpl, "{n}", group(int64(count)))
}

// pluralForms looks up a counted phrase.
func pluralForms(l Lang, key string) (PluralForms, bool) {
	f, ok := plurals[l][key]
	return f, ok
}

// group formats an integer with a comma every three digits.
// Western digits in both languages: these values are copyable identifiers.
func group(v int64) string {
	s := strconv.FormatInt(v, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// groupFloat formats a float with a fixed number of decimals, grouping the
// integer part.
func groupFloat(v float64, decimals int) string {
	s := strconv.FormatFloat(v, 'f', decimals, 64)
	intPart, frac, found := strings.Cut(s, ".")
	n, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return s
	}
	out := group(n)
	if found {
		out += "." + frac
	}
	return out
}

// Bytes humanizes a byte count with binary units (used for memory, disk).
// The numeral/unit pair is returned as a plain string; templates wrap it in
// <bdi> when it sits inside Arabic text (UAX #9 neutral-character isolation).
func (l *Localizer) Bytes(n int64) string {
	if n <= 0 {
		return "—"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	units := "KMGTPE"
	if l.lang == AR {
		return fmt.Sprintf("%.1f %ciB", v, units[exp])
	}
	return fmt.Sprintf("%.1f %ciB", v, units[exp])
}
