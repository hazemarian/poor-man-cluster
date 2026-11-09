package views

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/views/i18n"
)

// The console has ONE way to explain an icon: a data-tip attribute painted by
// the stylesheet's ::after. These tests are the enforcement half of that
// decision — title= would work again, silently, on the next button anyone adds,
// and the two mechanisms drift (native titles do not appear on keyboard focus
// at all).

var (
	// RE2 has no backreferences, so each control is matched by its own
	// close tag. None of these may nest in valid HTML.
	reControls = []*regexp.Regexp{
		regexp.MustCompile(`(?s)<button\b([^<>]*)>(.*?)</button>`),
		regexp.MustCompile(`(?s)<a\b([^<>]*)>(.*?)</a>`),
		regexp.MustCompile(`(?s)<summary\b([^<>]*)>(.*?)</summary>`),
	}
	reAction = regexp.MustCompile(`\{\{(?:T|TF|TN)\s+"[^"]+"`)
	reSRText = regexp.MustCompile(`(?s)<span class="sr-only">.*?</span>`)
	// A card whose glyph is wrapped in its own span carries the tooltip there:
	// the span is the affordance's face, the card is the link.
	reIconWrap = regexp.MustCompile(`(?s)^\s*<span[^>]*data-tip="[^"]+"[^>]*>\s*\{\{icon "`)
)

// TestEveryIconControlHasATooltip walks every template and requires every
// control that renders an icon to carry a non-empty data-tip. Counts are logged
// per template so coverage is checkable page by page, not "somewhere".
func TestEveryIconControlHasATooltip(t *testing.T) {
	paths, err := fs.Glob(files, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	total, iconOnly := 0, 0
	for _, p := range paths {
		if strings.HasSuffix(p, "frag_icons.html") {
			continue // the sprite itself, not a control
		}
		b, err := fs.ReadFile(files, p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		src := string(b)
		with, missing := 0, 0
		for _, re := range reControls {
			for _, m := range re.FindAllStringSubmatchIndex(src, -1) {
				attrs := src[m[2]:m[3]]
				body := src[m[4]:m[5]]
				if !strings.Contains(body, `{{icon "`) {
					continue
				}
				if strings.Contains(attrs, "data-tip=") || reIconWrap.MatchString(body) {
					with++
					total++
					// The icon must not be the only thing a screen reader gets.
					if isIconOnly(body) {
						iconOnly++
						if !strings.Contains(attrs, "aria-label=") && !strings.Contains(body, `class="sr-only"`) {
							t.Errorf("%s:%d: icon-only control has no accessible name", p, lineOf(src, m[0]))
						}
					}
					continue
				}
				missing++
				t.Errorf("%s:%d: a control renders an icon with no data-tip", p, lineOf(src, m[0]))
			}
		}
		t.Logf("%-34s %2d controls with a tooltip, %d without", strings.TrimPrefix(p, "templates/"), with, missing)
	}
	if total == 0 {
		t.Fatal("no icon controls found at all — the walker is broken")
	}
	t.Logf("%d icon controls carry a tooltip, %d of them icon-only (named)", total, iconOnly)
}

// isIconOnly reports whether a control's visible content is just the glyph:
// whitespace, the sprite reference and (allowed) an sr-only label.
func isIconOnly(body string) bool {
	b := reSRText.ReplaceAllString(body, "")
	b = regexp.MustCompile(`\{\{icon "[a-z0-9-]+"\}\}`).ReplaceAllString(b, "")
	b = reAction.ReplaceAllString(b, "")
	b = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(b, "")
	return strings.TrimSpace(b) == ""
}

// TestNoNativeTitleTooltipsRemain guards the migration: a translated title= is
// a second tooltip mechanism that no keyboard user ever sees.
func TestNoNativeTitleTooltipsRemain(t *testing.T) {
	paths, err := fs.Glob(files, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		b, err := fs.ReadFile(files, p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if i := strings.Index(string(b), `title="{{T`); i >= 0 {
			t.Errorf("%s:%d: still uses title=\"{{T …}}\"; use data-tip", p, lineOf(string(b), i))
		}
	}
}

// TestTooltipMechanismIsDefinedOnce pins the properties that make the mechanism
// work rather than merely exist: the text comes from the attribute, the box
// wraps instead of running off the line, the show delay stops the strobe while
// the pointer crosses a row of buttons, and the anchored (fixed) positioning —
// which is what escapes the table and nav scrollers — falls back to absolute
// where anchor positioning is unavailable.
func TestTooltipMechanismIsDefinedOnce(t *testing.T) {
	b, err := fs.ReadFile(StaticFS, "static/components.css")
	if err != nil {
		t.Fatalf("read components.css: %v", err)
	}
	css := string(b)
	for _, want := range []struct{ needle, why string }{
		{"[data-tip]::after", "the tooltip box must be the pseudo-element of the trigger"},
		{"content: attr(data-tip)", "the text must come from the attribute"},
		{"position: fixed", "fixed positioning is what escapes .table-wrap and .nav"},
		{"position-anchor: --tip", "the fixed box must be anchored back to its trigger"},
		{"position-try-fallbacks:", "a tooltip near a viewport edge must flip, not run off"},
		{"@position-try --tip-below", "the flip-below fallback must exist"},
		{"max-inline-size:", "sentences must wrap, not stretch across the viewport"},
		{"white-space: normal", "long tooltips must wrap"},
		{"pointer-events: none", "the tooltip must never eat the click it explains"},
		{"[data-tip]:focus-visible::after", "the tooltip must appear on keyboard focus"},
		{"[data-tip]:focus-within::after", "a wrapper around a disabled control must show it too"},
		{"transition-delay: .3s", "hover needs a show delay so crossing a row does not strobe"},
		{"@supports not (position-anchor: --tip)", "older engines need the absolute fallback"},
		{".needs-manager", "the disabled storage control keeps its own muted look"},
	} {
		if !strings.Contains(css, want.needle) {
			t.Errorf("components.css missing %q — %s", want.needle, want.why)
		}
	}
	// One mechanism: no second implementation hiding elsewhere.
	for _, p := range []string{"static/patterns.css", "static/base.css"} {
		b, err := fs.ReadFile(StaticFS, p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if strings.Contains(string(b), "data-tip") {
			t.Errorf("%s defines its own data-tip styling; the mechanism lives in components.css", p)
		}
	}
}

// TestTooltipStringsExistInBothLanguages covers the two keys this change adds;
// dict_parity_test.go enforces the wider key set.
func TestTooltipStringsExistInBothLanguages(t *testing.T) {
	en, ar := i18n.New(i18n.EN), i18n.New(i18n.AR)
	for _, key := range []string{"st.leader_tip", "st.storage_tip", "nodes.storage_needs_manager"} {
		if !en.Has(key) {
			t.Errorf("en: %q missing", key)
		}
		if !ar.Has(key) {
			t.Errorf("ar: %q missing", key)
		}
	}
	if en.T("st.leader_tip") == ar.T("st.leader_tip") {
		t.Error("st.leader_tip is identical in both languages")
	}
	// The inline label this task removed must not survive as a key.
	for _, l := range []*i18n.Localizer{en, ar} {
		if l.Has("nodes.storage_needs_manager_short") {
			t.Errorf("%s: nodes.storage_needs_manager_short still exists; the inline label is gone", l.Lang())
		}
	}
	// RTL: the sentence is Arabic, so it must be written in Arabic.
	if !strings.ContainsAny(ar.T("nodes.storage_needs_manager"), "ابتثجحخدذرزسشصضطظعغفقكلمنهوي") {
		t.Error("the Arabic needs-manager sentence is not in Arabic")
	}
}
