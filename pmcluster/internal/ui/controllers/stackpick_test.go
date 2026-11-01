package controllers

import (
	"net/http"
	"strings"
	"testing"
)

// The console used to offer service-scope configs/secrets only from a stack page
// — a page that exists only after a successful deploy. A manifest needs them
// before its first deploy, so a brand-new stack could never get them created from
// the UI at all. These routes close that loop: the stack name is typed, and it is
// allowed to name a stack the cluster has never seen.
func TestServiceScopeFormsAcceptAStackThatDoesNotExist(t *testing.T) {
	h := newCtrlHarness(t)
	settings := Settings{Controller: h.ctrl}
	h.engine.GET("/web/settings/secrets/for-stack", settings.SecretForStack)
	h.engine.GET("/web/settings/configs/for-stack", settings.ConfigForStack)

	const ghost = "adam-world" // no stack row, no configs, no secrets

	// Step 1: the picker, with a free-text field and suggestions (not a select).
	rr := h.get(t, "/web/settings/secrets/for-stack")
	if rr.Code != http.StatusOK {
		t.Fatalf("picker status = %d, want 200", rr.Code)
	}
	picker := rr.Body.String()
	if !strings.Contains(picker, `name="stack"`) {
		t.Errorf("picker has no stack field:\n%s", picker)
	}
	if !strings.Contains(picker, `list="stackpicker-stacks"`) {
		t.Errorf("picker does not offer stack suggestions")
	}
	if strings.Contains(picker, "<select") {
		t.Errorf("picker must be free text — a select cannot name a stack that does not exist yet")
	}

	// Step 2: the real form, posted at the stack-scoped endpoint.
	for _, tc := range []struct {
		path, wantAction, wantField string
	}{
		{"/web/settings/secrets/for-stack?stack=" + ghost, "/web/stacks/" + ghost + "/secrets/add", `name="value"`},
		{"/web/settings/configs/for-stack?stack=" + ghost, "/web/stacks/" + ghost + "/configs/add", `name="content"`},
	} {
		rr := h.get(t, tc.path)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body: %s", tc.path, rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if !strings.Contains(body, tc.wantAction) {
			t.Errorf("%s: form does not post to %s:\n%s", tc.path, tc.wantAction, body)
		}
		if !strings.Contains(body, ghost) {
			t.Errorf("%s: form does not name the %s stack", tc.path, ghost)
		}
		if !strings.Contains(body, tc.wantField) {
			t.Errorf("%s: wrong form — missing %s", tc.path, tc.wantField)
		}
	}
}
