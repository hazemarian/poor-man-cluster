package views

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// The shared banner takes caller-supplied blocks through partial; this renders every call site's
// partial so a typo in one surfaces here instead of as a 500 on a page the tests do not load.
func TestBannerPartialsRender(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/web/x", nil)
	tm := r.withRequest(c)

	type sk struct{ Hostname, Version string }
	data := map[string]any{
		"Stack": "s", "Name": "s", "Result": map[string]any{"Stack": "s", "Revision": 3, "Changed": true},
		"Apply":  map[string]any{"Warnings": []string{"w1"}},
		"Skew":   []sk{{"h", "v"}},
		"Detail": map[string]any{"Stack": map[string]any{"Name": "s", "Failover": map[string]any{"FromNode": "a", "ToNode": "b"}}},
		"ErrRaw": "raw", "ErrPermission": true, "ErrSection": "x", "ErrSectionTitle": "settings.title", "GitHubErr": "e",
	}
	for name, want := range map[string]string{
		"backups_runnow": "hx-post", "tls_site_action": "site/new", "deploy_result_extra": "<p>", "deploy_result_actions": "revisions/3",
		"stack_failover_actions": "/move", "overview_apply_list": "<li>w1", "overview_skew_list": "<li>h", "settings_err_extra": "b-raw",
		"settings_github_err": "b-raw", "backupbrowse_back": "/web/backups", "webhookdeliveries_back": "/web/webhooks",
	} {
		var b strings.Builder
		if err := tm.ExecuteTemplate(&b, name, data); err != nil {
			t.Errorf("%s: %v", name, err)
		} else if !strings.Contains(b.String(), want) {
			t.Errorf("%s: output missing %q: %s", name, want, b.String())
		}
	}
	for _, d := range []map[string]any{
		{"Tone": "warn", "Icon": "alert", "Title": "t"},
		{"Tone": "ok", "Icon": "check-circle", "Title": "t", "Dismiss": true, "Role": "status"},
	} {
		var b strings.Builder
		if err := tm.ExecuteTemplate(&b, "banner", d); err != nil || strings.Contains(b.String(), "no value") {
			t.Errorf("banner %v: %v %s", d, err, b.String())
		}
	}
}
