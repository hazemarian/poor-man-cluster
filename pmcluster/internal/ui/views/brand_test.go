package views

import (
	"io/fs"
	"strings"
	"testing"
)

var accents = []string{"orange", "blue", "violet", "rose", "cyan"}

// TestAccentTokensExist asserts base.css defines the accent system: the --a-* swatch tokens, the
// navy-on-accent text token, and one block per accent in BOTH themes. A swatch missing its light
// block would leave the dark text step on a light page and fail contrast.
func TestAccentTokensExist(t *testing.T) {
	b, err := fs.ReadFile(StaticFS, "static/base.css")
	if err != nil {
		t.Fatalf("read base.css: %v", err)
	}
	s := string(b)

	for _, tok := range []string{"--a-fill", "--a-light", "--a-deep", "--a-text", "--a-text-hover", "--on-accent", "--m-a", "--m-l", "--m-d"} {
		if !strings.Contains(s, tok+":") {
			t.Errorf("base.css missing token %q", tok)
		}
	}
	for _, a := range accents {
		dark := `:root[data-accent="` + a + `"] {`
		light := `:root[data-accent="` + a + `"][data-theme="light"] {`
		if !strings.Contains(s, dark) {
			t.Errorf("base.css missing the dark block for accent %q", a)
		}
		if !strings.Contains(s, light) {
			t.Errorf("base.css missing the light block for accent %q", a)
		}
	}
	if !strings.Contains(s, `[data-theme="light"] {`) {
		t.Errorf("base.css missing the light theme block")
	}
}

// TestLogoMarkStaticAsset asserts the standalone SVG mark ships as a static asset: the three-node
// mark with its outer links, in the brand orange.
func TestLogoMarkStaticAsset(t *testing.T) {
	b, err := fs.ReadFile(StaticFS, "static/logo-mark.svg")
	if err != nil {
		t.Fatalf("read logo-mark.svg: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `viewBox="0 0 100 100"`) {
		t.Errorf("logo-mark.svg must be the 100x100 three-node mark")
	}
	if !strings.Contains(s, "#FF8A00") {
		t.Errorf("logo-mark.svg must carry the brand orange")
	}
}

// TestTemplatesRenderBrandMark asserts the shell, sign-in and setup pages draw the shared mark
// symbol and the two-part wordmark, and that the symbol itself ships in the icon sprite.
func TestTemplatesRenderBrandMark(t *testing.T) {
	for _, name := range []string{"templates/app.html", "templates/login.html", "templates/setup.html"} {
		b, err := fs.ReadFile(files, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		s := string(b)
		for _, want := range []string{`href="#brandmark"`, `brand.name_lead`, `brand.name_accent`, `data-accent="{{ACCENT}}"`} {
			if !strings.Contains(s, want) {
				t.Errorf("%s missing %q", name, want)
			}
		}
	}

	b, err := fs.ReadFile(files, "templates/frag_icons.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `<symbol id="brandmark" viewBox="0 0 100 100">`) {
		t.Errorf("frag_icons.html must define the brandmark symbol")
	}
}

// TestAccentBootAndPickerPresent asserts the accent boot script (applied before first paint), the
// Settings picker and the persistence key are wired in.
func TestAccentBootAndPickerPresent(t *testing.T) {
	app, err := fs.ReadFile(files, "templates/app.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(app), "pmc_accent") {
		t.Errorf("app.html boot script must apply the persisted accent (pmc_accent)")
	}

	settings, err := fs.ReadFile(files, "templates/frag_settings.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accents {
		if !strings.Contains(string(settings), `name="pmc-accent" value="`+a+`"`) {
			t.Errorf("settings fragment missing the %q accent option", a)
		}
	}

	js, err := fs.ReadFile(StaticFS, "static/preferences.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "pmc_accent") {
		t.Errorf("preferences.js must persist the accent to localStorage and a cookie (pmc_accent)")
	}
}
