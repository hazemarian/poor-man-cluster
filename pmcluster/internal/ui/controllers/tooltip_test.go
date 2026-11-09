package controllers

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// The tooltip mechanism is CSS-only, so the only thing a test can prove is what
// the server actually emits: every icon inside a control carries a non-empty,
// translated data-tip, and the storage hint is a tooltip instead of a label.
// These tests render through the real controllers and template set — the same
// bytes a browser gets — rather than reading the template source.

var (
	reTip      = regexp.MustCompile(`data-tip="([^"]*)"`)
	reIconSVG  = regexp.MustCompile(`<svg class="ic"`)
	reMissingK = regexp.MustCompile(`⟦[^⟧]*⟧`) // the missing-translation marker
)

// getHX renders a route as HTMX would (bare fragment, no app shell).
func (h *ctrlHarness) getHX(t *testing.T, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://x"+path, nil)
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	h.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", path, rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

// iconParentsWithoutTip finds every sprite icon whose immediately enclosing tag
// has no data-tip. The icon is always the first child of the element that
// "faces" it, so the tag that closed last before the <svg is that element.
func iconParentsWithoutTip(html string) []string {
	var out []string
	idx := 0
	for {
		i := strings.Index(html[idx:], `<svg class="ic"`)
		if i < 0 {
			return out
		}
		pos := idx + i
		prevEnd := strings.LastIndex(html[:pos], ">")
		if prevEnd < 0 {
			idx = pos + 1
			continue
		}
		prevStart := strings.LastIndex(html[:prevEnd], "<")
		parent := html[prevStart : prevEnd+1]
		if !strings.Contains(parent, "data-tip=") {
			out = append(out, parent)
		}
		idx = pos + 1
	}
}

// TestRenderedPagesCarryIconTooltips renders every page the console mounts and
// asserts, per page: the tooltips are present, none is empty, none is an
// untranslated key, and every icon inside a button/link has one.
func TestRenderedPagesCarryIconTooltips(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountOverview()
	h.mountStacks()
	h.mountServices()
	h.mountPlatform()
	h.mountBackups()
	h.mountAPIKeys()
	h.mountSettings()
	h.mountWebhooks()
	h.mountUsage()
	h.mountTLS()

	for _, path := range []string{
		"/web/overview",
		"/web/stacks",
		"/web/stacks/demo",
		"/web/services",
		"/web/platform",
		"/web/backups",
		"/web/apikeys",
		"/web/settings",
		"/web/webhooks",
		"/web/usage",
		"/web/tls",
	} {
		body := h.getHX(t, path)

		tips := reTip.FindAllStringSubmatch(body, -1)
		if len(tips) == 0 {
			t.Errorf("%s: rendered no tooltips at all", path)
			continue
		}
		for _, m := range tips {
			if strings.TrimSpace(m[1]) == "" {
				t.Errorf("%s: an empty data-tip rendered", path)
			}
			if strings.Contains(m[1], "{{") || reMissingK.MatchString(m[1]) {
				t.Errorf("%s: tooltip is untranslated: %q", path, m[1])
			}
		}

		// Every icon that sits directly inside a control must be explained.
		for _, parent := range iconParentsWithoutTip(body) {
			if strings.HasPrefix(parent, "<button") || strings.HasPrefix(parent, "<a ") || strings.HasPrefix(parent, "<summary") {
				t.Errorf("%s: control rendered an icon with no tooltip: %s", path, truncate(parent, 90))
			}
		}

		t.Logf("%-16s %3d tooltips, %3d sprite icons", path, len(tips), len(reIconSVG.FindAllString(body, -1)))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestRenderedShellCarriesIconTooltips covers the chrome that is not part of
// any fragment: the sidebar (whose labels vanish in the rail), the top bar's
// language/theme/palette controls, sign-out and the dialog close button. The
// full-page render is what includes app.html.
func TestRenderedShellCarriesIconTooltips(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountOverview()
	body := h.get(t, "/web/overview").Body.String()

	for _, want := range []struct{ needle, why string }{
		{`data-tip="Overview"`, "sidebar/tab-bar nav entry"},
		{`data-tip="Log out"`, "sign-out control"},
		{`data-tip="Switch to dark mode"`, "theme toggle (light page)"},
		{`data-tip="Colour palette"`, "palette picker"},
		{`data-tip="Switch to Arabic"`, "language switch"},
		{`data-tip="Close"`, "dialog close button"},
		{`data-tip="Menu"`, "phone menu button"},
	} {
		if !strings.Contains(body, want.needle) {
			t.Errorf("app shell missing %s (%s)", want.needle, want.why)
		}
	}
	for _, parent := range iconParentsWithoutTip(body) {
		if strings.HasPrefix(parent, "<button") || strings.HasPrefix(parent, "<a ") || strings.HasPrefix(parent, "<summary") {
			t.Errorf("shell control rendered an icon with no tooltip: %s", truncate(parent, 90))
		}
	}
	// In Arabic the same controls carry Arabic tooltips, not English ones.
	ar := h.get(t, "/web/overview?lang=ar").Body.String()
	if !strings.Contains(ar, `data-tip="تسجيل الخروج"`) {
		t.Error("the Arabic shell is missing the Arabic sign-out tooltip")
	}
}

// TestStorageHintIsATooltipNotALabel is the operator's actual complaint: a
// worker row printed the words "needs manager" into the table. The row must now
// render the lock glyph alone, with the sentence in data-tip and in the
// accessible name, and the control must still read as unavailable.
func TestStorageHintIsATooltipNotALabel(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountOverview()
	body := h.getHX(t, "/web/overview")

	if strings.Contains(body, "needs manager") {
		t.Error("the worker row still renders the inline \"needs manager\" label")
	}
	const sentence = "Only a manager can be a storage node"
	if !strings.Contains(body, sentence) {
		t.Fatalf("the storage explanation is not in the rendered page at all:\n%s", body)
	}

	// Two rows carry the disabled storage control (the table, and the phone kv
	// list), so the same explanation must appear twice — once per rendering.
	if n := strings.Count(body, `class="needs-manager"`); n != 2 {
		t.Errorf("rendered %d disabled storage controls, want 2 (table + phone list)", n)
	}
	for _, want := range []string{
		`data-tip="` + sentence,
		`aria-label="` + sentence,
		`role="button"`,
		`aria-disabled="true"`,
		`tabindex="0"`, // reachable by keyboard, which is what raises the tooltip
		`<svg class="ic"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("disabled storage control missing %s", want)
		}
	}
	// The old inline short label key must not survive anywhere in the page.
	if strings.Contains(body, "storage_needs_manager_short") {
		t.Error("the removed short label key still renders")
	}

	// The explanation is a sentence in each language, not the same English
	// string with an Arabic attribute.
	arBody := h.getHX(t, "/web/overview?lang=ar")
	if !strings.Contains(arBody, `data-tip="لا تكون عقدة التخزين إلا مديراً`) {
		t.Error("the Arabic page does not carry the Arabic storage tooltip")
	}
	// RTL must not fall back to the English sentence.
	if strings.Contains(arBody, sentence) {
		t.Error("the Arabic page still contains the English storage sentence")
	}
}
