package i18n

import "testing"

func TestCategoryArabic(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "zero"}, {1, "one"}, {2, "two"},
		{3, "few"}, {10, "few"}, {11, "many"}, {99, "many"}, {100, "other"},
		{103, "few"}, {111, "many"}, {200, "other"}, {1001, "other"},
	}
	for _, c := range cases {
		if got := category(AR, c.n); got != c.want {
			t.Errorf("category(ar, %d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestCategoryEnglish(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{0, "other"}, {1, "one"}, {2, "other"}, {25, "other"}} {
		if got := category(EN, tc.n); got != tc.want {
			t.Errorf("category(en, %d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// The six Arabic categories must all render, and the dual must inflect for
// case — this is the rule the arabic-ui skill checks by hand.
func TestPluralFormsArabic(t *testing.T) {
	f := PluralForms{
		Zero:   "لا توجد نسخ احتياطية",
		One:    "نسخة احتياطية واحدة",
		TwoNom: "نسختان احتياطيتان",
		TwoObl: "نسختين احتياطيتين",
		Few:    "{n} نسخ احتياطية",
		Many:   "{n} نسخةً احتياطية",
		Other:  "{n} نسخة احتياطية",
	}
	cases := []struct {
		n    int
		c    Case
		want string
	}{
		{0, Nominative, "لا توجد نسخ احتياطية"},
		{1, Nominative, "نسخة احتياطية واحدة"},
		{2, Nominative, "نسختان احتياطيتان"},
		{2, Oblique, "نسختين احتياطيتين"},
		{3, Nominative, "3 نسخ احتياطية"},
		{10, Nominative, "10 نسخ احتياطية"},
		{11, Nominative, "11 نسخةً احتياطية"},
		{99, Nominative, "99 نسخةً احتياطية"},
		{100, Nominative, "100 نسخة احتياطية"},
		{1200, Nominative, "1,200 نسخة احتياطية"},
	}
	for _, c := range cases {
		if got := formatPlural(c.n, f, c.c, AR); got != c.want {
			t.Errorf("formatPlural(ar, %d, %s) = %q, want %q", c.n, c.c, got, c.want)
		}
	}
}

// English must not leak Arabic categories: 2 stacks is "other", not a dual.
func TestPluralFormsEnglish(t *testing.T) {
	f := PluralForms{One: "{n} stack", Other: "{n} stacks"}
	if got := formatPlural(2, f, Nominative, EN); got != "2 stacks" {
		t.Errorf("en 2 = %q, want %q", got, "2 stacks")
	}
	if got := formatPlural(1, f, Nominative, EN); got != "1 stack" {
		t.Errorf("en 1 = %q, want %q", got, "1 stack")
	}
}

// A form missing its fallback must never render empty text.
func TestPluralDegradesToOther(t *testing.T) {
	if got := formatPlural(7, PluralForms{Other: "{n} x"}, Nominative, AR); got != "7 x" {
		t.Errorf("fallback = %q, want %q", got, "7 x")
	}
}

func TestGroup(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{{0, "0"}, {7, "7"}, {999, "999"}, {1000, "1,000"}, {1234567, "1,234,567"}, {-4200, "-4,200"}} {
		if got := group(tc.in); got != tc.want {
			t.Errorf("group(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseLang(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Lang
	}{{"ar", AR}, {"ar-EG", AR}, {"ar_SA", AR}, {"AR", AR}, {"en-GB", EN}, {"fr", EN}, {"", EN}} {
		if got := Parse(tc.in); got != tc.want {
			t.Errorf("Parse(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDirection(t *testing.T) {
	if d := New(AR).Dir(); d != "rtl" {
		t.Errorf("ar dir = %q, want rtl", d)
	}
	if d := New(EN).Dir(); d != "ltr" {
		t.Errorf("en dir = %q, want ltr", d)
	}
}

// An untranslated key must be visible, never a silent blank.
func TestMissingKeyIsVisible(t *testing.T) {
	if got := New(AR).T("does.not.exist"); got != "⟦does.not.exist⟧" {
		t.Errorf("missing = %q", got)
	}
	if got := New(AR).TN("does.not.exist"); got != "" {
		t.Errorf("optional missing = %q, want empty", got)
	}
}
