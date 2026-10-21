package views

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestEveryIconTokenHasASpriteSymbol guards a silent failure: {{icon "name"}}
// emits <use href="#i-name"> and renders an EMPTY svg when the sprite has no
// such symbol. Nothing errors, nothing logs — the nav entry simply has no icon,
// which is exactly how the observability link shipped without one.
func TestEveryIconTokenHasASpriteSymbol(t *testing.T) {
	sprite, err := os.ReadFile(filepath.Join("templates", "frag_icons.html"))
	if err != nil {
		t.Fatalf("read sprite: %v", err)
	}
	symbols := map[string]bool{}
	for _, m := range regexp.MustCompile(`id="i-([a-z0-9-]+)"`).FindAllStringSubmatch(string(sprite), -1) {
		symbols[m[1]] = true
	}
	if len(symbols) == 0 {
		t.Fatal("no symbols found in the sprite")
	}

	files, err := filepath.Glob(filepath.Join("templates", "*.html"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list templates: %v", err)
	}
	use := regexp.MustCompile(`\{\{icon "([a-z0-9-]+)"\}\}`)
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range use.FindAllStringSubmatch(string(body), -1) {
			if !symbols[m[1]] {
				t.Errorf("%s uses {{icon %q}} but the sprite has no #i-%s — it renders an empty svg",
					filepath.Base(f), m[1], m[1])
			}
		}
	}
}
