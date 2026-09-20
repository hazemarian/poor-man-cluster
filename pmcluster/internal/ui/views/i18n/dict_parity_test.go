package i18n

import (
	"sort"
	"strings"
	"testing"
)

// The console ships two languages of equal standing. A key that exists in one
// and not the other renders as the marker ⟦key⟧ to half the operators, which is
// exactly the defect this suite exists to catch — so parity is asserted, not
// reviewed by eye.
//
// Namespace exceptions are deliberate and listed here: a namespace that is
// genuinely one-language (proper nouns, upstream error text) must be declared
// rather than silently tolerated.
var parityExempt = map[string]string{
	// No current exemptions. Add "prefix." here only with a comment saying why
	// the concept cannot be expressed in the other language.
}

func TestDictionariesCoverTheSameKeys(t *testing.T) {
	inEN := keysMissingFrom(EN, AR)
	inAR := keysMissingFrom(AR, EN)

	for _, k := range inEN {
		if exempt(k) {
			continue
		}
		t.Errorf("key %q is defined in English but not Arabic", k)
	}
	for _, k := range inAR {
		if exempt(k) {
			continue
		}
		t.Errorf("key %q is defined in Arabic but not English", k)
	}
}

func TestPluralFormsCoverTheSameKeys(t *testing.T) {
	for k := range plurals[EN] {
		if _, ok := plurals[AR][k]; !ok && !exempt(k) {
			t.Errorf("plural %q is defined in English but not Arabic", k)
		}
	}
	for k := range plurals[AR] {
		if _, ok := plurals[EN][k]; !ok && !exempt(k) {
			t.Errorf("plural %q is defined in Arabic but not English", k)
		}
	}
}

// TestArabicPluralFormsAreComplete is the CLDR requirement the arabic-ui skill
// calls out: Arabic has six categories, and an operator count of 2 or 11 must
// not fall back to the English-shaped singular/plural split.
func TestArabicPluralFormsAreComplete(t *testing.T) {
	required := []string{"zero", "one", "two", "few", "many", "other"}
	for key, forms := range plurals[AR] {
		have := map[string]string{
			"zero": forms.Zero, "one": forms.One, "two": forms.TwoNom,
			"few": forms.Few, "many": forms.Many, "other": forms.Other,
		}
		for _, cat := range required {
			if strings.TrimSpace(have[cat]) == "" {
				t.Errorf("plural %q (ar): category %s is empty — Arabic needs all six", key, cat)
			}
		}
		// The dual has a nominative and an oblique form; the skill requires the
		// case distinction wherever the counted noun sits in the oblique case
		// (after غير, بين, من, مع). Say which keys still lack it rather than
		// failing: a nominative-only dual is grammatical when the noun is the
		// subject of the sentence.
		if strings.TrimSpace(forms.TwoObl) == "" {
			t.Logf("plural %q (ar): no oblique dual (TwoObl) — fine only if the noun is never in the oblique case", key)
		}
	}
}

func keysMissingFrom(have, want Lang) []string {
	var out []string
	for k := range dicts[have] {
		if _, ok := dicts[want][k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func exempt(key string) bool {
	for prefix := range parityExempt {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}
