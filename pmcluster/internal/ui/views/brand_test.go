package views

import (
	"io/fs"
	"strings"
	"testing"
)

// TestBrandTokensExist asserts the brand stylesheet maps the design tokens for
// both themes and all six palettes: the palette accent tokens (--brand-*) and
// the theme surface/text tokens (--brand-bg-*/--brand-text-*/--brand-border).
func TestBrandTokensExist(t *testing.T) {
	b, err := fs.ReadFile(StaticFS, "static/brand.css")
	if err != nil {
		t.Fatalf("read brand.css: %v", err)
	}
	s := string(b)

	for _, tok := range []string{
		"--brand-primary", "--brand-secondary", "--brand-dark", "--brand-surface", "--brand-light",
		"--brand-bg-primary", "--brand-bg-secondary", "--brand-bg-elevated",
		"--brand-text-primary", "--brand-text-secondary", "--brand-border",
	} {
		if !strings.Contains(s, tok) {
			t.Errorf("brand.css missing token %q", tok)
		}
	}
	for _, theme := range []string{`[data-theme="dark"]`, `[data-theme="light"]`} {
		if !strings.Contains(s, theme) {
			t.Errorf("brand.css missing theme selector %q", theme)
		}
	}
	// Every palette selector is present and carries a distinct primary accent.
	palettes := map[string]string{
		"orange": "#FF5722",
		"purple": "#7C4DFF",
		"blue":   "#2979FF",
		"red":    "#FF1744",
		"green":  "#00E676",
		"teal":   "#1DE9B6",
	}
	for name, hex := range palettes {
		if !strings.Contains(s, `[data-palette="`+name+`"]`) {
			t.Errorf("brand.css missing palette selector [data-palette=%q]", name)
		}
		if !strings.Contains(s, hex) {
			t.Errorf("brand.css missing palette primary %q for %q", hex, name)
		}
	}
}

// TestLogoMarkStaticAsset asserts the standalone SVG mark ships as a static
// asset and carries the palette-aware stroke/fill reference.
func TestLogoMarkStaticAsset(t *testing.T) {
	b, err := fs.ReadFile(StaticFS, "static/logo-mark.svg")
	if err != nil {
		t.Fatalf("read logo-mark.svg: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "var(--brand-primary, #FF5722)") {
		t.Errorf("logo-mark.svg must reference var(--brand-primary)")
	}
	if !strings.Contains(s, `viewBox="0 0 120 120"`) {
		t.Errorf("logo-mark.svg must be the 120x120 isometric mark")
	}
}

// TestTemplatesReferenceLogoPartial asserts the shell, sign-in and setup pages
// render the combination-mark lockup, and that the partial carries the
// wordmark plus the inlined palette-aware mark.
func TestTemplatesReferenceLogoPartial(t *testing.T) {
	for _, name := range []string{"templates/app.html", "templates/login.html", "templates/setup.html"} {
		b, err := fs.ReadFile(files, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(string(b), `{{template "logo"}}`) {
			t.Errorf("%s must render the logo lockup partial", name)
		}
	}

	b, err := fs.ReadFile(files, "templates/frag_logo.html")
	if err != nil {
		t.Fatalf("read frag_logo.html: %v", err)
	}
	s := string(b)
	for _, want := range []string{
		"Poor Man's", "Cluster", "pmcluster",
		"var(--brand-primary, #FF5722)", `viewBox="0 0 120 120"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("frag_logo.html missing %q", want)
		}
	}
}

// TestPaletteBootAndPickerPresent asserts the palette boot script (applied
// before first paint), the picker markup and the persistence key are wired in.
func TestPaletteBootAndPickerPresent(t *testing.T) {
	app, err := fs.ReadFile(files, "templates/app.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(app), "pmc_palette") {
		t.Errorf("app.html boot script must apply the persisted palette (pmc_palette)")
	}

	pref, err := fs.ReadFile(files, "templates/frag_preferences.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"data-palette-picker", "data-palette-toggle",
		`data-palette-choose="orange"`, `data-palette-choose="purple"`,
		`data-palette-choose="blue"`, `data-palette-choose="red"`,
		`data-palette-choose="green"`, `data-palette-choose="teal"`,
	} {
		if !strings.Contains(string(pref), want) {
			t.Errorf("preferences fragment missing %q", want)
		}
	}

	js, err := fs.ReadFile(StaticFS, "static/preferences.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "pmc_palette") {
		t.Errorf("preferences.js must persist the palette to localStorage (pmc_palette)")
	}
}
