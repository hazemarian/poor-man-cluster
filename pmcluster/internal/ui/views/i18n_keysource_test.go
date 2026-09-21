package views

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/views/i18n"
)

// A key that a controller hands to a template at render time (d.MsgKey, d.ErrKey)
// never appears inside a template, so TestEveryTemplateKeyIsTranslated cannot see
// it. A typo, or a dictionary entry renamed without its caller, therefore leaks the
// raw key into the UI — on a path that only runs when something succeeds or fails,
// which is exactly where a screenshot sweep does not look.
//
// This scans the Go sources for key-shaped literals instead. It is a text scan, not
// a type-aware one, so it errs towards being forgiving: a literal that looks like a
// hostname or a filename is skipped.
var (
	reKeyLiteral = regexp.MustCompile(`"([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+)"`)
	reNotAKey    = regexp.MustCompile(
		`(?i)\.(?:com|net|org|io|dev|site|art|local|internal|test|example|app|` +
			`pem|crt|cer|key|yml|yaml|json|toml|txt|md|html|css|js|map|gz|zip|db|sql)$`)
)

func TestControllerKeysAreTranslated(t *testing.T) {
	var paths []string
	for _, dir := range []string{"../controllers", "."} {
		if _, err := os.Stat(dir); err != nil {
			t.Skipf("source tree not available from %s: %v", dir, err)
		}
		matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		for _, m := range matches {
			// Keys reachable only from a test are not user-visible.
			if strings.HasSuffix(m, "_test.go") {
				continue
			}
			paths = append(paths, m)
		}
	}
	if len(paths) == 0 {
		t.Fatal("no Go sources found to scan")
	}

	en, ar := i18n.New(i18n.EN), i18n.New(i18n.AR)
	seen := map[string]bool{}

	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		text := string(src)
		for _, m := range reKeyLiteral.FindAllStringSubmatchIndex(text, -1) {
			key := text[m[2]:m[3]]
			if reNotAKey.MatchString(key) || seen[key] {
				continue
			}
			seen[key] = true
			if !en.Has(key) {
				t.Errorf("%s:%d: %q is used in Go but missing from the English dictionary",
					p, lineOf(text, m[0]), key)
				continue
			}
			if !ar.Has(key) {
				t.Errorf("%s:%d: %q is used in Go but missing from the Arabic dictionary",
					p, lineOf(text, m[0]), key)
			}
		}
	}
}
