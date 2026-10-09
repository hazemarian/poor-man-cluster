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
