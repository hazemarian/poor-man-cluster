package views

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/views/i18n"
)

// The console ships two languages. A missing key does not throw: it renders a
// visible ⟦key⟧ marker, which is easy to miss in a screenshot and easy to catch
// in CI. These tests are the enforcement half of docs/console-i18n-contract.md.

var (
	reKeyCall    = regexp.MustCompile(`\{\{-?\s*(?:T|TN|TF)\s+"([^"{]+)"`)
	rePluralCall = regexp.MustCompile(`\{\{-?\s*P\s+\S+\s+"([^"{]+)"`)
)

// TestNewRendererParsesAllTemplates is the first thing to break when a fragment
// is edited: templates are parsed once at process start, and a stray {{end}}
// takes the whole console down rather than one page.
func TestNewRendererParsesAllTemplates(t *testing.T) {
	if _, err := NewRenderer(); err != nil {
		t.Fatalf("templates do not parse: %v", err)
	}
}

// TestEveryTemplateKeyIsTranslated walks the embedded templates for literal
// translation calls and requires both languages to define them.
func TestEveryTemplateKeyIsTranslated(t *testing.T) {
	paths, err := fs.Glob(files, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no templates found in the embedded FS")
	}

	en, ar := i18n.New(i18n.EN), i18n.New(i18n.AR)
	seen := map[string]bool{}

	for _, p := range paths {
		src, err := fs.ReadFile(files, p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		text := string(src)

		for _, m := range reKeyCall.FindAllStringSubmatchIndex(text, -1) {
			key := text[m[2]:m[3]]
			if seen[p+"|"+key] {
				continue
			}
			seen[p+"|"+key] = true
			if !en.Has(key) {
				t.Errorf("%s:%d: {{T %q}} has no English translation", p, lineOf(text, m[0]), key)
				continue
			}
			if !ar.Has(key) {
				t.Errorf("%s:%d: {{T %q}} has no Arabic translation", p, lineOf(text, m[0]), key)
			}
		}

		for _, m := range rePluralCall.FindAllStringSubmatchIndex(text, -1) {
			key := text[m[2]:m[3]]
			if seen["P|"+p+"|"+key] {
				continue
			}
			seen["P|"+p+"|"+key] = true
			for _, l := range []*i18n.Localizer{en, ar} {
				// Plural falls back to T, so an unregistered plural key shows
				// up as the ⟦key⟧ marker instead of a phrase.
				if strings.Contains(l.Plural(3, key), "⟦") {
					t.Errorf("%s:%d: {{P %q}} is not registered for language %q",
						p, lineOf(text, m[0]), key, l.Lang())
				}
			}
		}
	}
}

func lineOf(text string, idx int) int {
	return strings.Count(text[:idx], "\n") + 1
}
