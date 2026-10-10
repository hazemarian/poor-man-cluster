package views

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestAppTemplateReferencesLocalHtmx asserts the app shell loads htmx from the
// vendored static path rather than the unpkg CDN (which could be blocked or
// tampered with on an air-gapped host).
func TestAppTemplateReferencesLocalHtmx(t *testing.T) {
	b, err := files.ReadFile("templates/app.html")
	if err != nil {
		t.Fatalf("read app.html: %v", err)
	}
	s := string(b)
	if strings.Contains(s, "unpkg.com") {
		t.Errorf("app.html still references the unpkg CDN:\n%s", s)
	}
	if !strings.Contains(s, `/web/static/htmx.min.js`) {
		t.Errorf("app.html must reference the vendored /web/static/htmx.min.js")
	}
}

// TestStaticHandlerServesVendoredHtmx asserts the embedded static file is
// reachable at /web/static/htmx.min.js.
func TestStaticHandlerServesVendoredHtmx(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET(StaticBase+"/*filepath", StaticHandler())

	req := httptest.NewRequest(http.MethodGet, "/web/static/htmx.min.js", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("htmx.min.js = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "htmx") {
		t.Errorf("htmx.min.js body does not look like htmx (missing marker)")
	}
}

// TestTabBarHasSingleDefinition guards the shell's mobile tab bar. base.css owns
// the chrome and sizes the bar's columns from the item count
// (grid-auto-flow: column). A second definition in another stylesheet once
// pinned the bar to a fixed column count, so a destination added later wrapped
// into an uneven implicit column. Keep the definition in exactly one sheet.
func TestTabBarHasSingleDefinition(t *testing.T) {
	sheets := []string{"base.css", "components.css", "patterns.css"}
	var owners []string
	for _, name := range sheets {
		b, err := StaticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatalf("read static/%s: %v", name, err)
		}
		if strings.Contains(string(b), ".tabbar") {
			owners = append(owners, name)
		}
	}
	if len(owners) != 1 {
		t.Fatalf("the shell tab bar must be styled from exactly one stylesheet, found %d: %v", len(owners), owners)
	}
	if owners[0] != "base.css" {
		t.Errorf("the shell tab bar belongs to base.css (the shell chrome), not %s", owners[0])
	}
}
